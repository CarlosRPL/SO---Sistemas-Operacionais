package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────
//  SINGLE-THREAD — Pesquisa de Livros
//
//  As buscas chegam via HTTP e são enfileiradas
//  num slice. Um loop processa UMA busca por vez.
//  Enquanto processa, as demais aguardam na fila.
// ─────────────────────────────────────────

type SearchTask struct {
	ID         int
	Query      string
	ResultChan chan SearchResult // canal para devolver o resultado
}

type SearchResult struct {
	Query   string   `json:"query"`
	Results []string `json:"results"`
	Total   int      `json:"total"`
	Took    string   `json:"took"`
}

var (
	books       []string
	taskQueue   []SearchTask
	taskCounter int
)

func loadBooks(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			books = append(books, line)
		}
	}
	log.Printf("%d livros carregados.", len(books))
	return scanner.Err()
}

func binarySearch(query string) int {
	lower := strings.ToLower(query)
	return sort.Search(len(books), func(i int) bool {
		return strings.ToLower(books[i]) >= lower
	})
}

// searchBooks busca por correspondência parcial em toda a lista
// usa binary search como ponto de partida para otimizar
func searchBooks(query string) []string {
	if query == "" {
		return books
	}
	lower := strings.ToLower(query)
	var found []string

	start := binarySearch(query)
	for i := start; i < len(books); i++ {
		if strings.Contains(strings.ToLower(books[i]), lower) {
			found = append(found, books[i])
		}
	}
	for i := 0; i < start; i++ {
		if strings.Contains(strings.ToLower(books[i]), lower) {
			found = append(found, books[i])
		}
	}
	return found
}

// processTask executa a busca e devolve o resultado pelo canal
func processTask(t SearchTask) {
	start := time.Now()
	log.Printf("[single] Processando busca #%d: %q", t.ID, t.Query)

	results := searchBooks(t.Query)

	t.ResultChan <- SearchResult{
		Query:   t.Query,
		Results: results,
		Total:   len(results),
		Took:    time.Since(start).String(),
	}
}

func runLoop() {
	for {
		if len(taskQueue) > 0 {
			task := taskQueue[0]
			taskQueue = taskQueue[1:]
			processTask(task)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	taskCounter++
	resultChan := make(chan SearchResult, 1)

	task := SearchTask{
		ID:         taskCounter,
		Query:      r.URL.Query().Get("q"),
		ResultChan: resultChan,
	}

	taskQueue = append(taskQueue, task)
	log.Printf("Busca #%d enfileirada. Fila atual: %d", task.ID, len(taskQueue))

	result := <-resultChan // aguarda o loop processar

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	if err := loadBooks("books.txt"); err != nil {
		log.Fatal("Erro ao carregar livros:", err)
	}

	go runLoop()

	http.HandleFunc("/search", handleSearch)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK — %d livros | fila: %d\n", len(books), len(taskQueue))
	})

	log.Println("Servidor single-thread rodando em :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

//   curl "http://localhost:8080/search?q=harry"
//   curl "http://localhost:8080/search?q=machado"
//   curl "http://localhost:8080/search?q=tolkien"
//   curl "http://localhost:8080/search?q="        