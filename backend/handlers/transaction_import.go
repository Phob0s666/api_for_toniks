package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/t0n1ks/go-react-angular-expense-tracker/backend/database"
	"github.com/t0n1ks/go-react-angular-expense-tracker/backend/models"
)

func ImportTransactions(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	defaultCategoryID, err := parseOptionalCategoryID(c.PostForm("category_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid category_id"})
		return
	}
	if defaultCategoryID != nil {
		var category models.Category
		if err := database.DB.Where("id = ? AND user_id = ?", *defaultCategoryID, userID).First(&category).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusNotFound, gin.H{"error": "Category not found or does not belong to you"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify category: " + err.Error()})
			}
			return
		}
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}

	uploadedFile, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to open uploaded file"})
		return
	}
	defer uploadedFile.Close()

	content, err := io.ReadAll(uploadedFile)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read uploaded file"})
		return
	}

	rows, err := parseImportRows(fileHeader.Filename, content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(rows) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No transaction rows found in file"})
		return
	}

	imported := 0
	skipped := 0
	createdCategories := 0
	categoryCache := map[string]uint{}
	for idx, row := range rows {
		lineNo := idx + 2

		amount, err := parseAmount(firstNonEmpty(
			row["amount"],
			row["sum"],
			row["сумма"],
			row["сума"],
			row["amount_alt"],
		))
		if err != nil || amount == 0 {
			skipped++
			_ = lineNo
			continue
		}

		dateRaw := firstNonEmpty(row["date"], row["дата"])
		parsedDate, err := parseImportDate(dateRaw)
		if err != nil {
			skipped++
			continue
		}

		description := strings.TrimSpace(firstNonEmpty(
			row["description"],
			row["details"],
			row["note"],
			row["описание"],
			row["опис операції"],
			row["примечание"],
			row["призначення"],
		))
		if len(description) > 255 {
			description = description[:255]
		}

		typeValue := strings.ToLower(strings.TrimSpace(firstNonEmpty(row["type"], row["тип"], row["operation"], row["операция"], row["операція"])))
		transactionType := "expense"
		if typeValue == "income" || typeValue == "дохід" || typeValue == "доход" || typeValue == "поступление" || typeValue == "надходження" || amount > 0 {
			transactionType = "income"
		}
		if amount < 0 {
			amount = -amount
		}

		categoryID, wasCreated, err := resolveImportCategoryID(
			userID.(uint),
			defaultCategoryID,
			firstNonEmpty(row["category"], row["категория"], row["категорія"], row["категорiя"]),
			categoryCache,
		)
		if err != nil {
			skipped++
			_ = lineNo
			continue
		}
		if wasCreated {
			createdCategories++
		}

		transaction := models.Transaction{
			UserID:      userID.(uint),
			CategoryID:  categoryID,
			Amount:      amount,
			Description: description,
			Date:        parsedDate,
			Type:        transactionType,
			IncomeType:  "one_time",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		if err := database.DB.Create(&transaction).Error; err != nil {
			skipped++
			continue
		}
		imported++
	}

	c.JSON(http.StatusOK, gin.H{"imported": imported, "skipped": skipped, "created_categories": createdCategories})
}

func parseImportRows(filename string, content []byte) ([]map[string]string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".xlsx" {
		return parseXLSXRows(content)
	}
	if ext != ".csv" && ext != ".txt" {
		return nil, fmt.Errorf("unsupported file format. Use .xlsx, .csv or .txt")
	}
	return parseCSVRows(content)
}

func parseXLSXRows(content []byte) ([]map[string]string, error) {
	readerAt := bytes.NewReader(content)
	archive, err := zip.NewReader(readerAt, int64(len(content)))
	if err != nil {
		return nil, fmt.Errorf("invalid xlsx file")
	}

	sharedStrings := []string{}
	if data, err := readZipFile(archive, "xl/sharedStrings.xml"); err == nil {
		sharedStrings = parseSharedStrings(data)
	}

	sheetPath := "xl/worksheets/sheet1.xml"
	if _, err := readZipFile(archive, sheetPath); err != nil {
		sheetPath = ""
		for _, file := range archive.File {
			if strings.HasPrefix(file.Name, "xl/worksheets/") && strings.HasSuffix(file.Name, ".xml") {
				sheetPath = file.Name
				break
			}
		}
		if sheetPath == "" {
			return nil, fmt.Errorf("xlsx worksheet not found")
		}
	}

	sheetData, err := readZipFile(archive, sheetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read xlsx worksheet")
	}

	table := parseSheetRows(sheetData, sharedStrings)
	return tableRowsToMaps(table), nil
}

func parseCSVRows(content []byte) ([]map[string]string, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	records, err := reader.ReadAll()
	if err != nil {
		reader = csv.NewReader(bytes.NewReader(content))
		reader.Comma = ';'
		records, err = reader.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("invalid csv file")
		}
	}

	return tableRowsToMaps(records), nil
}

func tableRowsToMaps(rows [][]string) []map[string]string {
	if len(rows) < 2 {
		return []map[string]string{}
	}

	headers := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		headers[i] = normalizeHeader(h)
	}

	result := []map[string]string{}
	for _, row := range rows[1:] {
		item := map[string]string{}
		hasValue := false
		for i, val := range row {
			if i >= len(headers) || headers[i] == "" {
				continue
			}
			trimmed := strings.TrimSpace(val)
			item[headers[i]] = trimmed
			if trimmed != "" {
				hasValue = true
			}
		}
		if hasValue {
			result = append(result, item)
		}
	}
	return result
}

func normalizeHeader(header string) string {
	normalized := strings.ToLower(strings.TrimSpace(header))
	normalized = strings.ReplaceAll(normalized, "_", " ")
	normalized = strings.ReplaceAll(normalized, "-", " ")
	normalized = strings.Join(strings.Fields(normalized), " ")

	switch normalized {
	case "amount", "sum", "сумма", "сума", "сума в валюті картки", "сума в валюте карты":
		return "amount"
	case "сума в валюті транзакції", "сума в валюте транзакции":
		return "amount_alt"
	case "date", "дата":
		return "date"
	case "description", "details", "comment", "note", "описание", "примечание", "призначення", "опис операції", "опис операции":
		return "description"
	case "type", "тип", "operation", "операция", "операція":
		return "type"
	case "category", "категория", "категорія", "категорiя", "cat":
		return "category"
	default:
		return normalized
	}
}

func parseAmount(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("empty amount")
	}

	compact := strings.ReplaceAll(value, "\u00A0", " ")
	compact = strings.ReplaceAll(compact, " ", "")
	compact = strings.ReplaceAll(compact, ",", ".")
	if parsed, err := strconv.ParseFloat(compact, 64); err == nil {
		return parsed, nil
	}

	numberPattern := regexp.MustCompile(`[-+]?\d[\d\s]*([.,]\d+)?`)
	match := numberPattern.FindString(value)
	if strings.TrimSpace(match) == "" {
		return 0, fmt.Errorf("invalid amount")
	}
	match = strings.ReplaceAll(match, " ", "")
	match = strings.ReplaceAll(match, ",", ".")
	return strconv.ParseFloat(match, 64)
}

func parseImportDate(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	if num, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64); err == nil && num > 20000 && num < 100000 {
		return excelSerialToTime(num), nil
	}
	formats := []string{"2006-01-02", "02.01.2006", "02/01/2006", "2006/01/02", "02-01-2006", "2006-01-02 15:04:05", "02.01.2006 15:04:05", time.RFC3339}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func excelSerialToTime(serial float64) time.Time {
	baseDate := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	totalSeconds := int64(serial * 86400)
	return baseDate.Add(time.Duration(totalSeconds) * time.Second)
}

type xlsxSST struct {
	SI []struct {
		T string `xml:"t"`
		R []struct {
			T string `xml:"t"`
		} `xml:"r"`
	} `xml:"si"`
}

func parseSharedStrings(content []byte) []string {
	var sst xlsxSST
	if err := xml.Unmarshal(content, &sst); err != nil {
		return []string{}
	}

	result := make([]string, 0, len(sst.SI))
	for _, si := range sst.SI {
		if si.T != "" {
			result = append(result, si.T)
			continue
		}
		parts := make([]string, 0, len(si.R))
		for _, run := range si.R {
			parts = append(parts, run.T)
		}
		result = append(result, strings.Join(parts, ""))
	}
	return result
}

type xlsxWorksheet struct {
	Rows []struct {
		Cells []struct {
			Ref      string `xml:"r,attr"`
			Type     string `xml:"t,attr"`
			Value    string `xml:"v"`
			InlineIS struct {
				T string `xml:"t"`
			} `xml:"is"`
		} `xml:"c"`
	} `xml:"sheetData>row"`
}

func parseSheetRows(content []byte, sharedStrings []string) [][]string {
	var ws xlsxWorksheet
	if err := xml.Unmarshal(content, &ws); err != nil {
		return [][]string{}
	}

	table := [][]string{}
	maxCol := 0

	for _, row := range ws.Rows {
		parsedRow := map[int]string{}
		for _, cell := range row.Cells {
			colIndex := cellRefToIndex(cell.Ref)
			if colIndex < 0 {
				continue
			}

			value := strings.TrimSpace(cell.Value)
			if cell.Type == "inlineStr" {
				value = strings.TrimSpace(cell.InlineIS.T)
			} else if cell.Type == "s" {
				sharedIndex, err := strconv.Atoi(value)
				if err == nil && sharedIndex >= 0 && sharedIndex < len(sharedStrings) {
					value = sharedStrings[sharedIndex]
				}
			}

			parsedRow[colIndex] = value
			if colIndex > maxCol {
				maxCol = colIndex
			}
		}

		current := make([]string, maxCol+1)
		for index, value := range parsedRow {
			current[index] = value
		}
		table = append(table, current)
	}

	return table
}

func cellRefToIndex(ref string) int {
	if ref == "" {
		return -1
	}
	letters := ""
	for _, char := range ref {
		if char >= 'A' && char <= 'Z' {
			letters += string(char)
		} else if char >= 'a' && char <= 'z' {
			letters += string(char - 32)
		} else {
			break
		}
	}
	if letters == "" {
		return -1
	}

	index := 0
	for _, char := range letters {
		index = index*26 + int(char-'A'+1)
	}
	return index - 1
}

func readZipFile(archive *zip.Reader, path string) ([]byte, error) {
	for _, file := range archive.File {
		if file.Name != path {
			continue
		}
		fileReader, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer fileReader.Close()
		return io.ReadAll(fileReader)
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func parseOptionalCategoryID(raw string) (*uint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	id, err := strconv.ParseUint(value, 10, 32)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("invalid category id")
	}
	parsed := uint(id)
	return &parsed, nil
}

func resolveImportCategoryID(userID uint, defaultCategoryID *uint, categoryName string, cache map[string]uint) (uint, bool, error) {
	name := strings.TrimSpace(categoryName)
	if name == "" {
		if defaultCategoryID != nil {
			return *defaultCategoryID, false, nil
		}
		name = "Imported"
	}
	if len(name) > 100 {
		name = name[:100]
	}

	cacheKey := strings.ToLower(name)
	if cachedID, ok := cache[cacheKey]; ok {
		return cachedID, false, nil
	}

	var category models.Category
	if err := database.DB.Where("user_id = ? AND LOWER(name) = LOWER(?)", userID, name).First(&category).Error; err == nil {
		cache[cacheKey] = category.ID
		return category.ID, false, nil
	} else if err != gorm.ErrRecordNotFound {
		return 0, false, err
	}

	category = models.Category{
		UserID:    userID,
		Name:      name,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := database.DB.Create(&category).Error; err != nil {
		return 0, false, err
	}
	cache[cacheKey] = category.ID
	return category.ID, true, nil
}
