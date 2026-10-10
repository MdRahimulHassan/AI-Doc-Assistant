package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	return body
}

func newCtx() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	return c, w
}

func TestOK(t *testing.T) {
	c, w := newCtx()
	OK(c, gin.H{"id": 1})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := decode(t, w)
	if body["success"] != true {
		t.Errorf("success = %v, want true", body["success"])
	}
	if _, ok := body["data"]; !ok {
		t.Error("expected a data key")
	}
	if _, ok := body["error"]; ok {
		t.Error("success response must not contain an error key")
	}
}

func TestError(t *testing.T) {
	c, w := newCtx()
	Error(c, http.StatusConflict, CodeEmailTaken, "email already registered")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	body := decode(t, w)
	if body["success"] != false {
		t.Errorf("success = %v, want false", body["success"])
	}
	if _, ok := body["data"]; ok {
		t.Error("error response must not contain a data key")
	}
	errBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected an error object")
	}
	if errBody["code"] != CodeEmailTaken {
		t.Errorf("code = %v, want %s", errBody["code"], CodeEmailTaken)
	}
	if !c.IsAborted() {
		t.Error("Error must abort the handler chain")
	}
}