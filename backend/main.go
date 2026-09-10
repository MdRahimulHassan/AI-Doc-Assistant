package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var db *pgxpool.Pool
var jwtSecret = []byte(getEnv("JWT_SECRET", "dev-secret-change-me"))
var openaiKey = getEnv("OPENAI_API_KEY", "")

func getEnv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// ---------- JWT ----------

type Claims struct {
	UserID int `json:"uid"`
	jwt.RegisteredClaims
}

func makeToken(userID int) (string, error) {
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

func authRequired(c *gin.Context) {
	header := c.GetHeader("Authorization")
	tokenStr := strings.TrimPrefix(header, "Bearer ")
	if tokenStr == "" {
		c.AbortWithStatusJSON(401, gin.H{"error": "missing token"})
		return
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		c.AbortWithStatusJSON(401, gin.H{"error": "invalid token"})
		return
	}
	c.Set("user_id", claims.UserID)
	c.Next()
}

// ---------- Auth handlers ----------

func signup(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)

	var userID int
	err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1,$2) RETURNING id`,
		req.Email, string(hash),
	).Scan(&userID)
	if err != nil {
		c.JSON(400, gin.H{"error": "email already in use"})
		return
	}

	token, _ := makeToken(userID)
	c.JSON(200, gin.H{"token": token})
}

func login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var userID int
	var hash string
	err := db.QueryRow(context.Background(),
		`SELECT id, password_hash FROM users WHERE email=$1`, req.Email,
	).Scan(&userID, &hash)
	if err != nil {
		c.JSON(401, gin.H{"error": "invalid credentials"})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		c.JSON(401, gin.H{"error": "invalid credentials"})
		return
	}

	token, _ := makeToken(userID)
	c.JSON(200, gin.H{"token": token})
}

// ---------- OpenAI helper ----------

func askLLM(prompt string) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+openaiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from LLM")
	}
	return result.Choices[0].Message.Content, nil
}

// ---------- Document handlers ----------

func uploadDocument(c *gin.Context) {
	userID := c.GetInt("user_id")

	var req struct {
		Title   string `json:"title" binding:"required"`
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var docID int
	err := db.QueryRow(context.Background(),
		`INSERT INTO documents (user_id, title, content) VALUES ($1,$2,$3) RETURNING id`,
		userID, req.Title, req.Content,
	).Scan(&docID)
	if err != nil {
		c.JSON(500, gin.H{"error": "could not save document"})
		return
	}

	c.JSON(200, gin.H{"id": docID, "status": "uploaded"})
}

func listDocuments(c *gin.Context) {
	userID := c.GetInt("user_id")
	rows, err := db.Query(context.Background(),
		`SELECT id, title, created_at FROM documents WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		c.JSON(500, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	var docs []gin.H
	for rows.Next() {
		var id int
		var title string
		var createdAt time.Time
		rows.Scan(&id, &title, &createdAt)
		docs = append(docs, gin.H{"id": id, "title": title, "created_at": createdAt})
	}
	c.JSON(200, gin.H{"documents": docs})
}

func getDocument(c *gin.Context) (int, string, string, error) {
	userID := c.GetInt("user_id")
	docID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return 0, "", "", fmt.Errorf("bad id")
	}
	var title, content string
	err = db.QueryRow(context.Background(),
		`SELECT title, content FROM documents WHERE id=$1 AND user_id=$2`, docID, userID,
	).Scan(&title, &content)
	return docID, title, content, err
}

func summarizeDocument(c *gin.Context) {
	_, title, content, err := getDocument(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "document not found"})
		return
	}

	prompt := fmt.Sprintf("Summarize the following document titled \"%s\" in a few concise sentences:\n\n%s", title, content)
	summary, err := askLLM(prompt)
	if err != nil {
		c.JSON(500, gin.H{"error": "LLM call failed: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{"summary": summary})
}

// Ask a question. If doc_id is given, answer from that one document.
// Otherwise, answer using all of the user's documents concatenated
// (fine for a small personal library; no vector search in this simplified build).
func askQuestion(c *gin.Context) {
	userID := c.GetInt("user_id")

	var req struct {
		Question string `json:"question" binding:"required"`
		DocID    *int   `json:"doc_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var contextText string
	if req.DocID != nil {
		var title, content string
		err := db.QueryRow(context.Background(),
			`SELECT title, content FROM documents WHERE id=$1 AND user_id=$2`, *req.DocID, userID,
		).Scan(&title, &content)
		if err != nil {
			c.JSON(404, gin.H{"error": "document not found"})
			return
		}
		contextText = fmt.Sprintf("[%s]\n%s", title, content)
	} else {
		rows, err := db.Query(context.Background(),
			`SELECT title, content FROM documents WHERE user_id=$1 ORDER BY created_at DESC LIMIT 5`, userID)
		if err != nil {
			c.JSON(500, gin.H{"error": "query failed"})
			return
		}
		defer rows.Close()
		var parts []string
		for rows.Next() {
			var title, content string
			rows.Scan(&title, &content)
			parts = append(parts, fmt.Sprintf("[%s]\n%s", title, content))
		}
		if len(parts) == 0 {
			c.JSON(200, gin.H{"answer": "You haven't uploaded any documents yet."})
			return
		}
		contextText = strings.Join(parts, "\n\n")
	}

	prompt := fmt.Sprintf(
		"Answer the question using only the context below. If the answer isn't in the context, say so.\n\nContext:\n%s\n\nQuestion: %s",
		contextText, req.Question,
	)
	answer, err := askLLM(prompt)
	if err != nil {
		c.JSON(500, gin.H{"error": "LLM call failed: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{"answer": answer})
}

// ---------- main ----------

func main() {
	ctx := context.Background()
	dbURL := getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/aidocassistant")

	var err error
	db, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	r := gin.Default()

	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	r.POST("/signup", signup)
	r.POST("/login", login)

	auth := r.Group("/api")
	auth.Use(authRequired)
	{
		auth.GET("/documents", listDocuments)
		auth.POST("/documents", uploadDocument)
		auth.POST("/documents/:id/summarize", summarizeDocument)
		auth.POST("/ask", askQuestion)
	}

	r.Run(":8080")
}
