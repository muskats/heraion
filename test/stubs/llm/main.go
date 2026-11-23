// Command llm-stub is an OpenAI-compatible chat endpoint that replays a
// script, for the kind smoke test (scripts/test-kind.sh). The script is a
// JSON list of turns, read from $LLM_STUB_SCRIPT, e.g.
//
//	[{"calls":[{"name":"k8s__pods_list_in_namespace","args":{"namespace":"heraion"}}]},
//	 {"final":"smoke ok"}]
//
// It starts over when the script is used up, so every run gets the same
// answers. GET /v1/models answers 200 for `heraion check --connect`.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/muskats/heraion/internal/llm/llmtest"
)

func main() {
	b, err := os.ReadFile(os.Getenv("LLM_STUB_SCRIPT"))
	if err != nil {
		log.Fatal(err)
	}
	var turns []llmtest.Turn
	if err := json.Unmarshal(b, &turns); err != nil {
		log.Fatalf("script: %v", err)
	}
	script := llmtest.Script(turns...)
	script.Loop = true
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", script)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stub","object":"model"}]}`))
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
