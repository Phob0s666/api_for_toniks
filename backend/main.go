package main

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/t0n1ks/go-react-angular-expense-tracker/backend/database"
	"github.com/t0n1ks/go-react-angular-expense-tracker/backend/handlers"
	"github.com/t0n1ks/go-react-angular-expense-tracker/backend/middleware"
)

func main() {
	database.Connect()

	router := gin.Default()

	allowOrigins := []string{"http://localhost:5173", "http://localhost"}
	if raw := os.Getenv("CORS_ORIGINS"); raw != "" {
		allowOrigins = strings.Split(raw, ",")
	}

	router.Use(cors.New(cors.Config{
		AllowOrigins:     allowOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	router.POST("/api/register", handlers.RegisterUser)
	router.POST("/api/login", handlers.LoginUser)

	protected := router.Group("/api")
	protected.Use(middleware.AuthMiddleware())
	{
		protected.POST("/categories", handlers.CreateCategory)
		protected.GET("/categories", handlers.GetCategories)
		protected.PUT("/categories/:id", handlers.UpdateCategory)
		protected.DELETE("/categories/:id", handlers.DeleteCategory)

		protected.POST("/transactions", handlers.CreateTransaction)
		protected.POST("/transactions/import", handlers.ImportTransactions)
		protected.GET("/transactions", handlers.GetTransactions)
		protected.GET("/transactions/:id", handlers.GetTransactionByID)
		protected.PUT("/transactions/:id", handlers.UpdateTransaction)
		protected.DELETE("/transactions/:id", handlers.DeleteTransaction)

		protected.GET("/profile", handlers.GetProfile)
		protected.PUT("/profile", handlers.UpdateProfile)
		protected.DELETE("/user", handlers.DeleteAccount)

		protected.GET("/summary/daily", handlers.GetDailySummary)
		protected.GET("/summary/period", handlers.GetPeriodSummary)
		protected.GET("/stats", handlers.GetPeriodSummary)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(router.Run(":" + port))
}
