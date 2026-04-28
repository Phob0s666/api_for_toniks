package handlers

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
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

	categoryID, err := strconv.ParseUint(c.PostForm("category_id"), 10, 32)
	if err != nil || categoryID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category_id is required"})
		return
	}

	var category models.Category
	if err := database.DB.Where("id = ? AND user_id = ?", uint(categoryID), userID).First(&category).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Category not found or does not belong to you"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify category: " + err.Error()})
		}
		return
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
	for idx, row := range rows {
		lineNo := idx + 2

		amount, err := parseAmount(firstNonEmpty(row["amount"], row["sum"], row["сумма"], row["сума"]))
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

		transaction := models.Transaction{
			UserID:      userID.(uint),
			CategoryID:  uint(categoryID),
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

	c.JSON(http.StatusOK, gin.H{"imported": imported, "skipped": skipped})
}

func parseImportRows(filename string, content []byte) ([]map[string]string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".csv" && ext != ".txt" {
		return nil, fmt.Errorf("unsupported file format. Export your Excel statement to CSV and upload .csv")
	}
	return parseCSVRows(content)
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
	case "amount", "sum", "сумма", "сума":
		return "amount"
	case "date", "дата":
		return "date"
	case "description", "details", "comment", "note", "описание", "примечание", "призначення":
		return "description"
	case "type", "тип", "operation", "операция", "операція":
		return "type"
	default:
		return normalized
	}
}

func parseAmount(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("empty amount")
	}
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, "\u00A0", "")
	value = strings.ReplaceAll(value, ",", ".")
	return strconv.ParseFloat(value, 64)
}

func parseImportDate(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty date")
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
