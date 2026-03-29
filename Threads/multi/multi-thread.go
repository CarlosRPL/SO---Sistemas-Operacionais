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
	"sync"
	"time"
)

// ─────────────────────────────────────────
//  MULTI-THREAD — Pesquisa de Livros
//
//  Um pool de workers processa as buscas
//  em paralelo. Cada worker é uma goroutine
//  independente consumindo o mesmo canal.
//  Ideal quando há muitas buscas simultâneas.
// ─────────────────────────────────────────

const NUM_WORKERS = 10

type SearchTask struct {
	ID         int
	Query      string
	ResultChan chan SearchResult
}

type SearchResult struct {
	Query    string   `json:"query"`
	Results  []string `json:"results"`
	Total    int      `json:"total"`
	Took     string   `json:"took"`
	WorkerID int      `json:"worker_id"` // mostra qual worker processou
}

var (
	books       []string
	taskChan    = make(chan SearchTask, 100)
	taskCounter int
	mu          sync.Mutex
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

// worker escuta o canal de tarefas e processa cada busca de forma independente
// vários workers rodam em paralelo — cada um é uma goroutine separada
func worker(id int) {
	for task := range taskChan {
		start := time.Now()
		log.Printf("[worker-%d] Processando busca #%d: %q", id, task.ID, task.Query)

		results := searchBooks(task.Query)

		task.ResultChan <- SearchResult{
			Query:    task.Query,
			Results:  results,
			Total:    len(results),
			Took:     time.Since(start).String(),
			WorkerID: id,
		}
	}
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	taskCounter++
	id := taskCounter
	mu.Unlock()

	resultChan := make(chan SearchResult, 1)

	task := SearchTask{
		ID:         id,
		Query:      r.URL.Query().Get("q"),
		ResultChan: resultChan,
	}

	taskChan <- task // envia para o pool — qualquer worker livre pega
	log.Printf("Busca #%d enviada ao pool. Canal: %d/%d", id, len(taskChan), cap(taskChan))

	result := <-resultChan // aguarda o worker responder

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	if err := loadBooks("books.txt"); err != nil {
		log.Fatal("Erro ao carregar livros:", err)
	}

	// inicia o pool de workers — cada um em sua goroutine
	for i := 1; i <= NUM_WORKERS; i++ {
		go worker(i)
	}

	http.HandleFunc("/search", handleSearch)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK — %d livros | workers: %d | canal: %d\n",
			len(books), NUM_WORKERS, len(taskChan))
	})

	log.Printf("Servidor multi-thread rodando em :8081 (%d workers)", NUM_WORKERS)
	log.Fatal(http.ListenAndServe(":8081", nil))
}

// Como testar:
//   go run main.go
//   curl "http://localhost:8081/search?q=harry"
//   curl "http://localhost:8081/search?q=machado"
//
// Observe o campo "worker_id" na resposta — mostra qual goroutine processou.
// Faça várias requisições simultâneas e veja workers diferentes respondendo:
//   for i in {1..8}; do curl "http://localhost:8081/search?q=o" & done
