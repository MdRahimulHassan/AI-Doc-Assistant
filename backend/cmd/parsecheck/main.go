// Command parsecheck runs the document parsers on one file and prints what
// they extract. A development tool for judging extraction quality.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/parsecheck <file.pdf|file.docx|file.txt>")
		return 2
	}
	path := os.Args[1]

	byExt := map[string]string{".pdf": parser.MimePDF, ".docx": parser.MimeDOCX, ".txt": parser.MimeTXT}
	mimeType, ok := byExt[strings.ToLower(filepath.Ext(path))]
	if !ok {
		fmt.Fprintln(os.Stderr, "unsupported extension (use .pdf, .docx or .txt)")
		return 2
	}
	p, _ := parser.ForType(mimeType)

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	start := time.Now()
	text, err := p.Parse(ctx, f, info.Size())
	took := time.Since(start).Round(time.Millisecond)

	if err != nil {
		kind := "other error"
		switch {
		case errors.Is(err, parser.ErrNoText):
			kind = "ErrNoText"
		case errors.Is(err, parser.ErrCorrupt):
			kind = "ErrCorrupt"
		case errors.Is(err, parser.ErrInvalidText):
			kind = "ErrInvalidText"
		case errors.Is(err, parser.ErrTextTooLarge):
			kind = "ErrTextTooLarge"
		}
		fmt.Printf("FAILED (%s) after %v\n  %v\n", kind, took, err)
		return 1
	}

	runes := []rune(text)
	fmt.Printf("OK in %v: %d characters, %d paragraphs\n", took, utf8.RuneCountInString(text), len(strings.Split(text, "\n\n")))
	fmt.Println("----- first 1500 characters -----")
	if len(runes) > 1500 {
		runes = runes[:1500]
	}
	fmt.Println(string(runes))
	return 0
}