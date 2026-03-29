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
//  ORIENTADO A EVENTOS — Pesquisa de Livros
//
//  Cada etapa da busca dispara um evento.
//  Handlers independentes reagem a cada evento.
//  O handler HTTP só publica — não sabe nada
//  sobre como a busca será executada.
//
//  Ciclo de vida:
//    SEARCH_REQUESTED → SEARCH_EXECUTING → SEARCH_COMPLETED
//                                        ↘ SEARCH_EMPTY (nenhum resultado)
// ─────────────────────────────────────────

type EventType string

const (
	EventSearchRequested EventType = "SEARCH_REQUESTED"
	EventSearchExecuting EventType = "SEARCH_EXECUTING"
	EventSearchCompleted EventType = "SEARCH_COMPLETED"
	EventSearchEmpty     EventType = "SEARCH_EMPTY" // evento especial: nenhum resultado
)

type SearchPayload struct {
	ID         int
	Query      string
	Results    []string
	Took       string
	ResultChan chan SearchResponse
}

type SearchResponse struct {
	Query   string   `json:"query"`
	Results []string `json:"results"`
	Total   int      `json:"total"`
	Took    string   `json:"took"`
	Event   string   `json:"last_event"` // mostra o evento que gerou a resposta
}

type Event struct {
	Type    EventType
	Payload SearchPayload
}

// EventBus gerencia os canais de cada tipo de evento
type EventBus struct {
	channels map[EventType]chan Event
	mu       sync.RWMutex
}

func NewEventBus() *EventBus {
	return &EventBus{
		channels: map[EventType]chan Event{
			EventSearchRequested: make(chan Event, 50),
			EventSearchExecuting: make(chan Event, 50),
			EventSearchCompleted: make(chan Event, 50),
			EventSearchEmpty:     make(chan Event, 50),
		},
	}
}

func (eb *EventBus) Publish(e Event) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	eb.channels[e.Type] <- e
}

func (eb *EventBus) Subscribe(t EventType) <-chan Event {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	return eb.channels[t]
}

// ─────────────────────────────────────────
//  DADOS
// ─────────────────────────────────────────

var books []string

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

// ─────────────────────────────────────────
//  HANDLERS DE EVENTO
// ─────────────────────────────────────────

// onSearchRequested: valida e encaminha para execução
func onSearchRequested(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchRequested) {
		log.Printf("[evento] SEARCH_REQUESTED → busca #%d: %q", event.Payload.ID, event.Payload.Query)
		go func(e Event) {
			bus.Publish(Event{Type: EventSearchExecuting, Payload: e.Payload})
		}(event)
	}
}

// onSearchExecuting: executa a busca de fato
func onSearchExecuting(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchExecuting) {
		go func(e Event) {
			start := time.Now()
			log.Printf("[evento] SEARCH_EXECUTING → buscando %q...", e.Payload.Query)

			results := searchBooks(e.Payload.Query)
			e.Payload.Results = results
			e.Payload.Took = time.Since(start).String()

			if len(results) == 0 {
				// nenhum resultado → evento especial
				bus.Publish(Event{Type: EventSearchEmpty, Payload: e.Payload})
			} else {
				bus.Publish(Event{Type: EventSearchCompleted, Payload: e.Payload})
			}
		}(event)
	}
}

// onSearchCompleted: devolve os resultados ao handler HTTP
func onSearchCompleted(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchCompleted) {
		log.Printf("[evento] SEARCH_COMPLETED → %d resultado(s) para %q",
			len(event.Payload.Results), event.Payload.Query)

		event.Payload.ResultChan <- SearchResponse{
			Query:   event.Payload.Query,
			Results: event.Payload.Results,
			Total:   len(event.Payload.Results),
			Took:    event.Payload.Took,
			Event:   string(EventSearchCompleted),
		}
	}
}

// onSearchEmpty: busca sem resultados — handler separado permite
// lógica diferente (sugestões, log de métricas, etc.)
func onSearchEmpty(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchEmpty) {
		log.Printf("[evento] SEARCH_EMPTY → nenhum resultado para %q", event.Payload.Query)

		event.Payload.ResultChan <- SearchResponse{
			Query:   event.Payload.Query,
			Results: []string{},
			Total:   0,
			Took:    event.Payload.Took,
			Event:   string(EventSearchEmpty),
		}
	}
}

// ─────────────────────────────────────────
//  HTTP + MAIN
// ─────────────────────────────────────────

var (
	bus         = NewEventBus()
	searchCounter int
	mu          sync.Mutex
)

func handleSearch(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	searchCounter++
	id := searchCounter
	mu.Unlock()

	resultChan := make(chan SearchResponse, 1)

	// o handler HTTP só publica o evento — não sabe nada sobre a busca
	bus.Publish(Event{
		Type: EventSearchRequested,
		Payload: SearchPayload{
			ID:         id,
			Query:      r.URL.Query().Get("q"),
			ResultChan: resultChan,
		},
	})

	result := <-resultChan // aguarda a cadeia de eventos concluir

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	if err := loadBooks("books.txt"); err != nil {
		log.Fatal("Erro ao carregar livros:", err)
	}

	// registra os handlers — cada um em sua goroutine
	go onSearchRequested(bus)
	go onSearchExecuting(bus)
	go onSearchCompleted(bus)
	go onSearchEmpty(bus)

	http.HandleFunc("/search", handleSearch)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK — %d livros | 4 event handlers ativos\n", len(books))
	})

	log.Println("Servidor orientado a eventos rodando em :8082")
	log.Fatal(http.ListenAndServe(":8082", nil))
}

// Como testar:
//   go run main.go
//   curl "http://localhost:8082/search?q=harry"     ← SEARCH_COMPLETED
//   curl "http://localhost:8082/search?q=tolkien"   ← SEARCH_COMPLETED
//   curl "http://localhost:8082/search?q=xyzabc"    ← SEARCH_EMPTY
//
// Observe o campo "last_event" na resposta — mostra qual evento gerou o retorno.
// Adicionar novo comportamento = novo handler, sem tocar no código existente.