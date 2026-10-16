// Command verify runs the Week 2 verification matrix against a RUNNING server
// and prints PASS/FAIL for each expectation. It checks the HTTP answers and
// also looks at the database and the upload folder directly.
//
//	go run ./cmd/verify
//	go run ./cmd/verify -big "E:\Downloads\paper.pdf" -query editorials
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
)

// ---------------------------------------------------------------------------
// types
// ---------------------------------------------------------------------------

type reply struct {
	Status  int
	Body    []byte
	Data    json.RawMessage
	ErrCode string
	Err     error // transport error: no response at all
}

func (r reply) String() string {
	switch {
	case r.Err != nil:
		msg := r.Err.Error()
		if len(msg) > 90 {
			msg = msg[:90] + "..."
		}
		return "no response (" + msg + ")"
	case r.ErrCode != "":
		return fmt.Sprintf("%d %s", r.Status, r.ErrCode)
	}
	return fmt.Sprint(r.Status)
}

type docJSON struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename"`
	MimeType     string  `json:"mime_type"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message"`
	ChunkCount   int     `json:"chunk_count"`
}

type user struct{ id, email, token string }

type owned struct{ token, id string }

type fixtures struct{ pdf, docx, txt, scanned []byte }

type ingestCase struct {
	name        string
	data        []byte
	queries     []string
	minChunks   int
	maxChunks   int
	minAvgWords int // only applied when the document has more than one chunk
}

type verifier struct {
	base      string
	client    *http.Client
	pool      *pgxpool.Pool
	uploadDir string
	maxUpload int64

	passed, failed int
	created        []owned // every document this run uploaded, removed at the end
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() { os.Exit(run()) }

func run() int {
	baseFlag := flag.String("base", "", "server URL (default: http://localhost:<SERVER_PORT from .env>)")
	fixturesDir := flag.String("fixtures", filepath.Join("internal", "parser", "testdata"), "folder with sample.pdf, sample.docx and sample.txt")
	bigPath := flag.String("big", "", "optional extra document (for example your long paper) to ingest and search")
	bigQuery := flag.String("query", "editorial", "a word that must be searchable in the -big document")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	fx, err := loadFixtures(*fixturesDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixtures:", err)
		return 2
	}
	pool, err := database.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		return 2
	}
	defer pool.Close()

	base := *baseFlag
	if base == "" {
		base = "http://localhost:" + cfg.Port
	}
	v := &verifier{
		base:      strings.TrimRight(base, "/"),
		client:    &http.Client{Timeout: 2 * time.Minute},
		pool:      pool,
		uploadDir: cfg.UploadDir,
		maxUpload: cfg.MaxUploadBytes,
	}

	if rep := v.do(http.MethodGet, "/health", "", nil, ""); rep.Err != nil || rep.Status != http.StatusOK {
		fmt.Fprintf(os.Stderr, "the server is not reachable at %s (%s)\nstart it first, from backend/:  go run ./cmd/server\n", v.base, rep)
		return 2
	}
	fmt.Printf("server %s | upload dir %s | upload limit %d bytes\n", v.base, v.uploadDir, v.maxUpload)

	a, err := v.newUser("a")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, err := v.newUser("b")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	c, err := v.newUser("c") // takes every rejected upload, so "nothing was stored" is easy to prove
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer v.cleanup([]string{a.email, b.email, c.email})

	txtDoc := v.item1(a, fx, *bigPath, *bigQuery)
	v.item2(c, fx)
	v.item3(c)
	v.nothingStored(c)
	v.item4(a, fx)
	v.item5(a, txtDoc, fx)
	v.item6(a, b, txtDoc, fx)
	v.item7(a, fx)
	v.item8(a, fx)
	v.item9(a)

	fmt.Printf("\nRESULT: %d passed, %d failed\n", v.passed, v.failed)
	if v.failed > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// the nine items
// ---------------------------------------------------------------------------

func (v *verifier) item1(a user, fx fixtures, bigPath, bigQuery string) docJSON {
	section(1, "Upload PDF, DOCX and TXT",
		`202 "processing", then "ready"; 1-3 chunks each; none over 600 words; the search finds the words`)

	var txtDoc docJSON
	for _, c := range []ingestCase{
		{name: "sample.pdf", data: fx.pdf},
		{name: "sample.docx", data: fx.docx},
		{name: "sample.txt", data: fx.txt},
	} {
		c.queries = []string{"zebrafish", "laboratory"} // "laboratory" matches "laboratories" through stemming
		c.minChunks, c.maxChunks = 1, 3
		if doc, ok := v.ingestAndSearch(a, c); ok && c.name == "sample.txt" {
			txtDoc = doc
		}
	}

	if bigPath != "" {
		data, err := os.ReadFile(bigPath)
		if err != nil {
			v.check("-big file", "a readable file", false, err.Error())
		} else {
			v.ingestAndSearch(a, ingestCase{
				name: filepath.Base(bigPath), data: data, queries: []string{bigQuery},
				minChunks: 1, maxChunks: 2000, minAvgWords: 150,
			})
		}
	}
	return txtDoc
}

func (v *verifier) item2(c user, fx fixtures) {
	section(2, "Wrong file types", "415 UNSUPPORTED_FILE_TYPE for every one; nothing stored")

	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff\x00\x00"), make([]byte, 1024)...)
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00"), make([]byte, 512)...)

	for _, tc := range []struct {
		label, name string
		data        []byte
	}{
		{"a Windows executable", "setup.exe", exe},
		{"a PNG image renamed .pdf", "photo.pdf", png},
		{"extra: a PDF renamed .docx", "report.docx", fx.pdf},
		{"extra: a spreadsheet-style ZIP renamed .docx", "sheet.docx", zipWith("xl/workbook.xml")},
		{"extra: text with a NUL byte named .txt", "notes.txt", []byte("abc\x00def")},
	} {
		v.expect(tc.label, v.upload(bearer(c.token), "file", tc.name, tc.data), http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE")
	}
}

func (v *verifier) item3(c user) {
	section(3, "Over the limit, and no file field",
		"413 FILE_TOO_LARGE (a hugely oversized body may get the connection closed instead); 400 NO_FILE")

	n := int(v.maxUpload)/5 + 1
	justOver := bytes.Repeat([]byte("word "), n) // a few bytes over the limit
	v.expect("a file just over the limit", v.upload(bearer(c.token), "file", "big.txt", justOver),
		http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE")

	// Far over the limit the server stops reading and may close the connection
	// while the client is still sending, so "no response" is also acceptable here.
	huge := bytes.Repeat([]byte("word "), int(v.maxUpload)/5*2)
	rep := v.upload(bearer(c.token), "file", "huge.txt", huge)
	ok := rep.Err != nil || (rep.Status == http.StatusRequestEntityTooLarge && rep.ErrCode == "FILE_TOO_LARGE")
	v.check("a file twice the limit", "413 FILE_TOO_LARGE, or the connection closed early", ok, rep.String())

	v.expect("form without a \"file\" field", v.upload(bearer(c.token), "note", "a.txt", []byte("hello")), http.StatusBadRequest, "NO_FILE")
	v.expect("an empty file", v.upload(bearer(c.token), "file", "empty.pdf", nil), http.StatusBadRequest, "NO_FILE")
	v.expect("extra: not multipart at all",
		v.do(http.MethodPost, "/api/documents", bearer(c.token), strings.NewReader("{}"), "application/json"),
		http.StatusBadRequest, "BAD_REQUEST")
}

// nothingStored proves that every upload rejected in items 2 and 3 left no trace.
func (v *verifier) nothingStored(c user) {
	ids, rep := v.listIDs(c.token)
	rows, err := v.scalar(`SELECT count(*)::int FROM documents WHERE user_id = $1`, uuid.MustParse(c.id))
	_, statErr := os.Stat(filepath.Join(v.uploadDir, c.id))
	ok := rep.Status == http.StatusOK && len(ids) == 0 && err == nil && rows == 0 && errors.Is(statErr, fs.ErrNotExist)
	v.check("rejected uploads left nothing behind", "no documents, no rows, no folder for this user", ok,
		fmt.Sprintf("%d listed, %d rows, folder exists: %v", len(ids), rows, statErr == nil))
}

func (v *verifier) item4(a user, fx fixtures) {
	section(4, "A PDF with no text layer (scanned)",
		`202, then "failed" with a message starting "No readable text"; 0 chunks`)

	doc, rep, ok := v.uploadDoc(a, "scan.pdf", fx.scanned)
	v.check("upload accepted", "202", ok, rep.String())
	if !ok {
		return
	}

	settled, took, done := v.waitSettled(a.token, doc.ID, 90*time.Second)
	msg := ""
	if settled.ErrorMessage != nil {
		msg = *settled.ErrorMessage
	}
	v.check("ends as failed with the friendly message", `status "failed", message starts "No readable text"`,
		done && settled.Status == "failed" && strings.HasPrefix(msg, "No readable text"),
		fmt.Sprintf("status %q after %v, message %q", settled.Status, took, msg))

	rows, err := v.scalar(`SELECT count(*)::int FROM chunks WHERE document_id = $1`, uuid.MustParse(doc.ID))
	v.check("no chunks stored", "0 chunk rows", err == nil && rows == 0 && settled.ChunkCount == 0,
		fmt.Sprintf("%d rows, chunk_count %d", rows, settled.ChunkCount))
}

func (v *verifier) item5(a user, doc docJSON, fx fixtures) {
	section(5, "No token", "401 UNAUTHORIZED on every protected route; the document is unaffected")
	if doc.ID == "" {
		v.check("needs the document from item 1", "a ready document", false, "item 1 produced none")
		return
	}
	path := "/api/documents/" + doc.ID

	v.expect("upload without a token", v.upload("", "file", "x.txt", fx.txt), http.StatusUnauthorized, "UNAUTHORIZED")
	v.expect("list without a token", v.do(http.MethodGet, "/api/documents", "", nil, ""), http.StatusUnauthorized, "UNAUTHORIZED")
	v.expect("get without a token", v.do(http.MethodGet, path, "", nil, ""), http.StatusUnauthorized, "UNAUTHORIZED")
	v.expect("delete without a token", v.do(http.MethodDelete, path, "", nil, ""), http.StatusUnauthorized, "UNAUTHORIZED")
	v.expect("a garbage token", v.do(http.MethodGet, "/api/documents", "Bearer not.a.token", nil, ""), http.StatusUnauthorized, "UNAUTHORIZED")
	v.expect("the wrong scheme", v.do(http.MethodGet, "/api/documents", "Basic dXNlcjpwYXNz", nil, ""), http.StatusUnauthorized, "UNAUTHORIZED")

	v.expect("the document is unaffected", v.do(http.MethodGet, path, bearer(a.token), nil, ""), http.StatusOK, "")
}

func (v *verifier) item6(a, b user, doc docJSON, fx fixtures) {
	section(6, "A second user",
		"404 NOT_FOUND for GET and DELETE of the first user's document; their list is empty; the first user's document and file are untouched")
	if doc.ID == "" {
		v.check("needs the document from item 1", "a ready document", false, "item 1 produced none")
		return
	}
	path := "/api/documents/" + doc.ID

	v.expect("second user: GET", v.do(http.MethodGet, path, bearer(b.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
	v.expect("second user: DELETE", v.do(http.MethodDelete, path, bearer(b.token), nil, ""), http.StatusNotFound, "NOT_FOUND")

	ids, rep := v.listIDs(b.token)
	v.check("second user's list is empty", `200 with "documents": []`,
		rep.Status == http.StatusOK && len(ids) == 0 && strings.Contains(string(rep.Body), `"documents":[]`),
		fmt.Sprintf("%s, %d document(s)", rep, len(ids)))

	v.expect("first user still reads the document", v.do(http.MethodGet, path, bearer(a.token), nil, ""), http.StatusOK, "")
	rel, err := v.storagePath(uuid.MustParse(doc.ID))
	v.check("first user's file is untouched", "the file is still on disk", err == nil && v.fileExists(rel), rel)

	// And the other way round.
	bd, rep, ok := v.uploadDoc(b, "bobs.txt", fx.txt)
	v.check("second user can upload", "202", ok, rep.String())
	if !ok {
		return
	}
	v.expect("first user cannot read the second user's document",
		v.do(http.MethodGet, "/api/documents/"+bd.ID, bearer(a.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
	aIDs, _ := v.listIDs(a.token)
	v.check("first user's list does not contain it", "the second user's ID is absent", !contains(aIDs, bd.ID), fmt.Sprintf("%d ids", len(aIDs)))
	bIDs, _ := v.listIDs(b.token)
	v.check("second user's list has exactly their document", "1 document", len(bIDs) == 1 && bIDs[0] == bd.ID, fmt.Sprintf("%d ids", len(bIDs)))
}

func (v *verifier) item7(a user, fx fixtures) {
	section(7, "Delete a document",
		"204 with an empty body; the row, its chunks and the file are all gone; later GET and DELETE give 404")

	doc, ok := v.ingestAndSearch(a, ingestCase{
		name: "to-delete.txt", data: fx.txt, queries: []string{"zebrafish"}, minChunks: 1, maxChunks: 3,
	})
	if !ok {
		return
	}
	id := uuid.MustParse(doc.ID)
	rel, err := v.storagePath(id)
	chunksBefore, _ := v.scalar(`SELECT count(*)::int FROM chunks WHERE document_id = $1`, id)
	v.check("before the delete: row, chunks and file exist", "all three present",
		err == nil && chunksBefore > 0 && v.fileExists(rel), fmt.Sprintf("%d chunks, file %q", chunksBefore, rel))

	path := "/api/documents/" + doc.ID
	rep := v.do(http.MethodDelete, path, bearer(a.token), nil, "")
	v.check("delete answers 204 with no body", "204 and an empty body", rep.Err == nil && rep.Status == http.StatusNoContent && len(rep.Body) == 0,
		fmt.Sprintf("%s, %d body bytes", rep, len(rep.Body)))

	rows, _ := v.scalar(`SELECT count(*)::int FROM documents WHERE id = $1`, id)
	chunks, _ := v.scalar(`SELECT count(*)::int FROM chunks WHERE document_id = $1`, id)
	v.check("the row is gone", "0 rows", rows == 0, fmt.Sprint(rows, " rows"))
	v.check("the chunks are gone", "0 rows", chunks == 0, fmt.Sprint(chunks, " rows"))
	v.check("the file is gone", "no file on disk", v.fileGone(rel), rel)

	v.expect("GET afterwards", v.do(http.MethodGet, path, bearer(a.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
	v.expect("DELETE again", v.do(http.MethodDelete, path, bearer(a.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
}

func (v *verifier) item8(a user, fx fixtures) {
	section(8, `Upload named "..\..\evil.pdf"`,
		"202; display name evil.pdf; stored as <user_id>/<doc_id>.pdf; no file named like evil anywhere")

	for _, name := range []string{`..\..\evil.pdf`, "../../evil.pdf", `C:\Windows\evil.pdf`} {
		doc, rep, ok := v.uploadDoc(a, name, fx.pdf)
		v.check(fmt.Sprintf("%q: accepted, shown as evil.pdf", name), `202 and filename "evil.pdf"`,
			ok && doc.Filename == "evil.pdf", fmt.Sprintf("%s, filename %q", rep, doc.Filename))
		if !ok {
			continue
		}
		rel, err := v.storagePath(uuid.MustParse(doc.ID))
		want := a.id + "/" + doc.ID + ".pdf"
		v.check(fmt.Sprintf("%q: stored under a generated path", name), want, err == nil && rel == want, rel)
		v.check(fmt.Sprintf("%q: the file is inside the upload folder", name), "file exists at that path", v.fileExists(rel), rel)
	}

	// Nothing may have been written where the client's name pointed.
	var stray []string
	for _, p := range []string{
		filepath.Join(v.uploadDir, "evil.pdf"),
		filepath.Join(v.uploadDir, "..", "evil.pdf"),
		filepath.Join(v.uploadDir, "..", "..", "evil.pdf"),
	} {
		if _, err := os.Stat(p); err == nil {
			stray = append(stray, p)
		}
	}
	_ = filepath.WalkDir(v.uploadDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(strings.ToLower(d.Name()), "evil") {
			stray = append(stray, p)
		}
		return nil
	})
	v.check("no file named like evil exists anywhere", "none found", len(stray) == 0, fmt.Sprintf("%d found %v", len(stray), stray))
}

func (v *verifier) item9(a user) {
	section(9, "Malformed document IDs", "404 NOT_FOUND for GET and DELETE, never 500")

	for _, id := range []string{"not-a-uuid", "12345", "' OR 1=1 --", uuid.NewString()} {
		path := "/api/documents/" + url.PathEscape(id)
		v.expect(fmt.Sprintf("GET %q", id), v.do(http.MethodGet, path, bearer(a.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
		v.expect(fmt.Sprintf("DELETE %q", id), v.do(http.MethodDelete, path, bearer(a.token), nil, ""), http.StatusNotFound, "NOT_FOUND")
	}
}

// ---------------------------------------------------------------------------
// one document, start to finish
// ---------------------------------------------------------------------------

func (v *verifier) ingestAndSearch(a user, c ingestCase) (docJSON, bool) {
	rep := v.upload(bearer(a.token), "file", c.name, c.data)
	doc, parsed := decodeDoc(rep)
	v.check(c.name+": upload accepted", `202 with status "processing"`,
		rep.Err == nil && rep.Status == http.StatusAccepted && parsed && doc.Status == "processing",
		fmt.Sprintf("%s, status %q", rep, doc.Status))
	if !parsed {
		return docJSON{}, false
	}
	v.created = append(v.created, owned{a.token, doc.ID})

	body := string(rep.Body)
	v.check(c.name+": response hides internals", "no storage_path or user_id in the response",
		!strings.Contains(body, "storage_path") && !strings.Contains(body, "user_id"), "checked")

	settled, took, done := v.waitSettled(a.token, doc.ID, 90*time.Second)
	got := fmt.Sprintf("status %q after %v", settled.Status, took)
	if settled.ErrorMessage != nil {
		got += fmt.Sprintf(", message %q", *settled.ErrorMessage)
	}
	v.check(c.name+": becomes ready", `status "ready"`, done && settled.Status == "ready", got)
	if !done || settled.Status != "ready" {
		return settled, false
	}

	id := uuid.MustParse(doc.ID)
	n, minW, maxW, avgW, err := v.chunkStats(id)
	sensible := err == nil && n == settled.ChunkCount && n >= c.minChunks && n <= c.maxChunks && maxW <= chunker.DefaultMaxWords
	if n > 1 && avgW < c.minAvgWords {
		sensible = false
	}
	v.check(c.name+": chunk counts look sensible",
		fmt.Sprintf("%d-%d chunks, none over %d words, API count equals the rows", c.minChunks, c.maxChunks, chunker.DefaultMaxWords),
		sensible,
		fmt.Sprintf("%d chunks (API says %d); words per chunk: min %d, avg %d, max %d", n, settled.ChunkCount, minW, avgW, maxW))

	for _, q := range c.queries {
		hits, err := v.searchHits(id, q)
		v.check(fmt.Sprintf("%s: search %q", c.name, q), "at least 1 matching chunk", err == nil && hits >= 1,
			fmt.Sprintf("%d matching chunk(s)", hits))
	}
	return settled, true
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func bearer(token string) string { return "Bearer " + token }

func (v *verifier) do(method, path, auth string, body io.Reader, contentType string) reply {
	req, err := http.NewRequest(method, v.base+path, body)
	if err != nil {
		return reply{Err: err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return reply{Err: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	rep := reply{Status: resp.StatusCode, Body: data}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if len(data) > 0 && json.Unmarshal(data, &env) == nil {
		rep.Data = env.Data
		if env.Error != nil {
			rep.ErrCode = env.Error.Code
		}
	}
	return rep
}

// upload sends a multipart form. auth is the complete Authorization header ("" for none).
func (v *verifier) upload(auth, field, filename string, content []byte) reply {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return reply{Err: err}
	}
	if _, err := part.Write(content); err != nil {
		return reply{Err: err}
	}
	if err := mw.Close(); err != nil {
		return reply{Err: err}
	}
	return v.do(http.MethodPost, "/api/documents", auth, &body, mw.FormDataContentType())
}

// uploadDoc uploads and remembers the document for cleanup. ok means "202 and a document came back".
func (v *verifier) uploadDoc(u user, name string, data []byte) (docJSON, reply, bool) {
	rep := v.upload(bearer(u.token), "file", name, data)
	doc, parsed := decodeDoc(rep)
	ok := rep.Err == nil && rep.Status == http.StatusAccepted && parsed
	if ok {
		v.created = append(v.created, owned{u.token, doc.ID})
	}
	return doc, rep, ok
}

func decodeDoc(rep reply) (docJSON, bool) {
	var d struct {
		Document docJSON `json:"document"`
	}
	if rep.Data == nil || json.Unmarshal(rep.Data, &d) != nil || d.Document.ID == "" {
		return docJSON{}, false
	}
	return d.Document, true
}

func (v *verifier) listIDs(token string) ([]string, reply) {
	rep := v.do(http.MethodGet, "/api/documents?limit=100", bearer(token), nil, "")
	var d struct {
		Documents []docJSON `json:"documents"`
	}
	_ = json.Unmarshal(rep.Data, &d)
	ids := make([]string, 0, len(d.Documents))
	for _, doc := range d.Documents {
		ids = append(ids, doc.ID)
	}
	return ids, rep
}

// waitSettled polls GET /api/documents/:id until the document leaves "processing".
func (v *verifier) waitSettled(token, id string, timeout time.Duration) (docJSON, time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < timeout {
		rep := v.do(http.MethodGet, "/api/documents/"+id, bearer(token), nil, "")
		if d, ok := decodeDoc(rep); ok && d.Status != "processing" {
			return d, time.Since(start).Round(time.Millisecond), true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return docJSON{}, timeout, false
}

func (v *verifier) newUser(prefix string) (user, error) {
	email := fmt.Sprintf("verify-%s-%s@example.com", prefix, uuid.NewString()[:8])
	creds, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})

	if rep := v.do(http.MethodPost, "/api/auth/register", "", bytes.NewReader(creds), "application/json"); rep.Err != nil || rep.Status != http.StatusCreated {
		return user{}, fmt.Errorf("register %s: %s", email, rep)
	}
	rep := v.do(http.MethodPost, "/api/auth/login", "", bytes.NewReader(creds), "application/json")
	var login struct {
		Token string `json:"token"`
	}
	if rep.Err != nil || rep.Status != http.StatusOK || json.Unmarshal(rep.Data, &login) != nil || login.Token == "" {
		return user{}, fmt.Errorf("login %s: %s", email, rep)
	}

	rep = v.do(http.MethodGet, "/api/me", bearer(login.Token), nil, "")
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if rep.Err != nil || json.Unmarshal(rep.Data, &me) != nil || me.User.ID == "" {
		return user{}, fmt.Errorf("/api/me for %s: %s", email, rep)
	}
	return user{id: me.User.ID, email: email, token: login.Token}, nil
}

// ---------------------------------------------------------------------------
// database and disk helpers
// ---------------------------------------------------------------------------

func (v *verifier) scalar(query string, args ...any) (int, error) {
	var n int
	err := v.pool.QueryRow(context.Background(), query, args...).Scan(&n)
	return n, err
}

func (v *verifier) chunkStats(id uuid.UUID) (n, minW, maxW, avgW int, err error) {
	err = v.pool.QueryRow(context.Background(), `
		SELECT count(*)::int,
		       coalesce(min(w), 0)::int,
		       coalesce(max(w), 0)::int,
		       coalesce(round(avg(w)), 0)::int
		FROM (SELECT cardinality(regexp_split_to_array(btrim(content), '\s+')) AS w
		      FROM chunks WHERE document_id = $1) t`, id).Scan(&n, &minW, &maxW, &avgW)
	return
}

// searchHits is the full-text query from the plan.
func (v *verifier) searchHits(id uuid.UUID, query string) (int, error) {
	return v.scalar(`SELECT count(*)::int FROM chunks
		WHERE document_id = $1 AND tsv @@ plainto_tsquery('english', $2)`, id, query)
}

func (v *verifier) storagePath(id uuid.UUID) (string, error) {
	var p string
	err := v.pool.QueryRow(context.Background(), `SELECT storage_path FROM documents WHERE id = $1`, id).Scan(&p)
	return p, err
}

func (v *verifier) fullPath(rel string) string {
	return filepath.Join(v.uploadDir, filepath.FromSlash(rel))
}

func (v *verifier) fileExists(rel string) bool {
	_, err := os.Stat(v.fullPath(rel))
	return err == nil
}

func (v *verifier) fileGone(rel string) bool {
	_, err := os.Stat(v.fullPath(rel))
	return errors.Is(err, fs.ErrNotExist)
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

func section(n int, title, expect string) {
	fmt.Printf("\n%d. %s\n   expect: %s\n", n, title, expect)
}

func (v *verifier) check(label, expected string, ok bool, got string) {
	if ok {
		v.passed++
		fmt.Printf("   [PASS] %s  (%s)\n", label, got)
		return
	}
	v.failed++
	fmt.Printf("   [FAIL] %s\n          expected: %s\n          got:      %s\n", label, expected, got)
}

func (v *verifier) expect(label string, rep reply, status int, code string) {
	want := fmt.Sprint(status)
	if code != "" {
		want += " " + code
	}
	ok := rep.Err == nil && rep.Status == status && (code == "" || rep.ErrCode == code)
	v.check(label, want, ok, rep.String())
}

func (v *verifier) cleanup(emails []string) {
	removed := 0
	for _, o := range v.created {
		rep := v.do(http.MethodDelete, "/api/documents/"+o.id, bearer(o.token), nil, "")
		if rep.Err == nil && rep.Status == http.StatusNoContent {
			removed++ // 404 means an earlier step already deleted it, which is fine
		}
	}
	tag, err := v.pool.Exec(context.Background(), `DELETE FROM users WHERE email = ANY($1)`, emails)
	users := int64(0)
	if err == nil {
		users = tag.RowsAffected()
	}
	fmt.Printf("\ncleanup: removed %d remaining test documents (and their files) and %d test users\n", removed, users)
}

// ---------------------------------------------------------------------------
// test data
// ---------------------------------------------------------------------------

func loadFixtures(dir string) (fixtures, error) {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }

	var fx fixtures
	var err error
	if fx.pdf, err = read("sample.pdf"); err != nil {
		return fx, err
	}
	if fx.docx, err = read("sample.docx"); err != nil {
		return fx, err
	}
	if fx.txt, err = read("sample.txt"); err != nil {
		return fx, err
	}
	// A real scanned.pdf is used if you made one; otherwise a generated PDF that
	// draws a rectangle and contains no text, which is the same thing to a text
	// extractor as a page that is only an image.
	if fx.scanned, err = read("scanned.pdf"); err != nil {
		fx.scanned = blankPagePDF()
	}
	return fx, nil
}

func zipWith(entry string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(entry)
	_, _ = w.Write([]byte("<x/>"))
	_ = zw.Close()
	return buf.Bytes()
}

// blankPagePDF builds a valid one-page PDF with no text at all.
func blankPagePDF() []byte {
	const content = "0.8 g 72 600 300 100 re f\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}