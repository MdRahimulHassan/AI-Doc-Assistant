package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDocument_JSONHidesInternalFields(t *testing.T) {
	d := Document{
		ID:          uuid.New(),
		UserID:      uuid.New(),
		Filename:    "a.pdf",
		StoragePath: "secret/dir/file.pdf",
		Status:      StatusReady,
	}

	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)

	for _, leaked := range []string{"secret/dir", "storage_path", "user_id", d.UserID.String()} {
		if strings.Contains(out, leaked) {
			t.Errorf("JSON leaks %q: %s", leaked, out)
		}
	}
}