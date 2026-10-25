// Command groqcheck calls the real Groq API through the llm client, once with
// Complete and once with Stream, using the settings in .env. With -n it fires
// several calls at once to show the pacer and the retry logic at work.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/llm"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

func main() { os.Exit(run()) }

func run() int {
	n := flag.Int("n", 1, "fire this many Complete calls at once (watch the pacer and retries; spends real tokens)")
	maxTokens := flag.Int("max", 400, "max output tokens per call")
	flag.Parse()

	prompt := strings.Join(flag.Args(), " ")
	if prompt == "" {
		prompt = "Say hello in five words."
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if !cfg.LLMEnabled() {
		fmt.Fprintln(os.Stderr, "GROQ_API_KEY is not set in .env")
		return 2
	}

	client, err := llm.New(llm.Config{APIKey: cfg.GroqAPIKey, TPMBudget: cfg.GroqTPMBudget, MaxConcurrent: cfg.LLMGlobalConcurrency})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		return 2
	}
	defer client.Close()

	req := service.ChatRequest{
		Model:           cfg.GroqModel,
		Messages:        []service.ChatMessage{{Role: "user", Content: prompt}},
		MaxOutputTokens: *maxTokens,
		Temperature:     0.2,
		ReasoningEffort: "low",
	}
	ctx := context.Background()
	fmt.Printf("model %s, token budget %d per minute\n", cfg.GroqModel, cfg.GroqTPMBudget)

	if *n > 1 {
		return burst(ctx, client, req, *n)
	}

	fmt.Println("\n--- Complete ---")
	start := time.Now()
	resp, err := client.Complete(ctx, req)
	if err != nil {
		report(err)
		return 1
	}
	fmt.Printf("%q\nfinish=%s usage=%+v took=%v\n", resp.Content, resp.FinishReason, resp.Usage, time.Since(start).Round(time.Millisecond))

	fmt.Println("\n--- Stream ---")
	start = time.Now()
	var first time.Duration
	pieces := 0
	resp, err = client.Stream(ctx, req, func(text string) error {
		if pieces == 0 {
			first = time.Since(start)
		}
		pieces++
		fmt.Print(text)
		return nil
	})
	if err != nil {
		fmt.Println()
		report(err)
		fmt.Printf("partial answer so far: %q\n", resp.Content)
		return 1
	}
	fmt.Printf("\n\n%d pieces of text, first after %v, total %v\nfinish=%s usage=%+v\n",
		pieces, first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), resp.FinishReason, resp.Usage)
	return 0
}

func burst(ctx context.Context, client *llm.Client, req service.ChatRequest, n int) int {
	fmt.Printf("\nfiring %d calls at once...\n\n", n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0

	start := time.Now()
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t0 := time.Now()
			resp, err := client.Complete(ctx, req)

			mu.Lock()
			defer mu.Unlock()
			took := time.Since(t0).Round(time.Millisecond)
			if err != nil {
				failed++
				var le *service.LLMError
				if errors.As(err, &le) {
					fmt.Printf("call %2d: FAILED after %v: %s (retry after %v)\n", i, took, le.Kind, le.RetryAfter.Round(time.Second))
				} else {
					fmt.Printf("call %2d: FAILED after %v: %v\n", i, took, err)
				}
				return
			}
			fmt.Printf("call %2d: ok after %v, %d tokens\n", i, took, resp.Usage.TotalTokens)
		}(i)
	}
	wg.Wait()
	fmt.Printf("\n%d ok, %d failed, %v in total\n", n-failed, failed, time.Since(start).Round(time.Millisecond))
	return 0
}

func report(err error) {
	var le *service.LLMError
	if errors.As(err, &le) {
		fmt.Printf("LLM error: kind=%s status=%d code=%q retry_after=%v\nmessage (logs only): %s\n",
			le.Kind, le.Status, le.Code, le.RetryAfter, le.Message)
		return
	}
	fmt.Println("error:", err)
}