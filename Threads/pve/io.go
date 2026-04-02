package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ─────────────────────────────────────────
//  ORIENTADO A EVENTOS — Pesquisa de Livros
//
//  1. loadBooks usa io_uring — I/O assíncrono verdadeiro em arquivo.
//     O processo submete a operação ao kernel e só processa quando
//     a completion queue sinaliza que terminou. Zero blocking.
//
//  2. handleSearch usa select + context.Done() — sem syscall bloqueante.
//     O Go runtime usa epoll internamente nos canais/sockets.
//
//  Ciclo de vida:
//    SEARCH_REQUESTED → SEARCH_EXECUTING → SEARCH_COMPLETED
//                                        ↘ SEARCH_EMPTY
// ─────────────────────────────────────────

// ─────────────────────────────────────────
//  io_uring — syscall numbers e structs
// ─────────────────────────────────────────
const (
	sysIoUringSetup  = 425
	sysIoUringEnter  = 426
	ioringOpRead     = 22
	ioringOffSqRing  = 0
	ioringOffCqRing  = 0x08000000
	ioringOffSqes    = 0x10000000
	ioringEnterGetev = 1
)

type sqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	flags       uint32
	dropped     uint32
	array       uint32
	_           uint32
	_           uint64
}

type cqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	overflow    uint32
	cqes        uint32
	flags       uint32
	_           uint32
	_           uint64
}

type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	_            [3]uint32
	sqOff        sqRingOffsets
	cqOff        cqRingOffsets
}

type ioUringSQE struct {
	opcode      uint8
	flags       uint8
	ioprio      uint16
	fd          int32
	off         uint64
	addr        uint64
	len         uint32
	opcodeFlags uint32
	userData    uint64
	_           [24]byte
}

type ioUringCQE struct {
	userData uint64
	res      int32
	flags    uint32
}

func ioUringSetup(entries uint32, params *ioUringParams) (int, error) {
	fd, _, errno := unix.Syscall(
		sysIoUringSetup,
		uintptr(entries),
		uintptr(unsafe.Pointer(params)),
		0,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(fd), nil
}

func ioUringEnter(ringFd int, toSubmit, minComplete, flags uint32) error {
	_, _, errno := unix.Syscall6(
		sysIoUringEnter,
		uintptr(ringFd),
		uintptr(toSubmit),
		uintptr(minComplete),
		uintptr(flags),
		0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// ─────────────────────────────────────────
//  EVENT BUS
// ─────────────────────────────────────────
type EventType string

const (
	EventSearchRequested EventType = "SEARCH_REQUESTED"
	EventSearchExecuting EventType = "SEARCH_EXECUTING"
	EventSearchCompleted EventType = "SEARCH_COMPLETED"
	EventSearchEmpty     EventType = "SEARCH_EMPTY"
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
	Event   string   `json:"last_event"`
}

type Event struct {
	Type    EventType
	Payload SearchPayload
}

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
//  DADOS — leitura assíncrona com io_uring
// ─────────────────────────────────────────
var books []string

// loadBooks usa io_uring: submete READ ao kernel e aguarda a
// completion queue sinalizar — sem bloquear nenhuma thread do OS.
func loadBooks(path string) error {
	// ── 1. Abre o arquivo ─────────────────────────────────────────────
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("unix.Open: %w", err)
	}
	defer unix.Close(fd)

	// ── 2. Tamanho do arquivo ─────────────────────────────────────────
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("Fstat: %w", err)
	}
	if stat.Size == 0 {
		return fmt.Errorf("arquivo vazio: %q", path)
	}
	buf := make([]byte, stat.Size)

	// ── 3. Inicializa io_uring ────────────────────────────────────────
	var params ioUringParams
	ringFd, err := ioUringSetup(1, &params)
	if err != nil {
		return fmt.Errorf("io_uring_setup: %w", err)
	}
	defer unix.Close(ringFd)

	// ── 4. Mapeia SQ ring em userspace ───────────────────────────────
	sqSize := uintptr(params.sqOff.array) + uintptr(params.sqEntries)*4
	sqRaw, err := unix.Mmap(ringFd, ioringOffSqRing, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQ ring: %w", err)
	}
	defer unix.Munmap(sqRaw)

	// ── 5. Mapeia SQEs ────────────────────────────────────────────────
	sqeSize := uintptr(params.sqEntries) * unsafe.Sizeof(ioUringSQE{})
	sqeRaw, err := unix.Mmap(ringFd, ioringOffSqes, int(sqeSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQEs: %w", err)
	}
	defer unix.Munmap(sqeRaw)

	// ── 6. Mapeia CQ ring em userspace ───────────────────────────────
cqSize := uintptr(params.cqOff.cqes) +
    uintptr(params.cqEntries)*unsafe.Sizeof(ioUringCQE{})
	cqRaw, err := unix.Mmap(ringFd, ioringOffCqRing, int(cqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap CQ ring: %w", err)
	}
	defer unix.Munmap(cqRaw)

	// ── 7. Preenche SQE: READ do arquivo inteiro ─────────────────────
	sqe := (*ioUringSQE)(unsafe.Pointer(&sqeRaw[0]))
	sqe.opcode   = ioringOpRead
	sqe.fd       = int32(fd)
	sqe.off      = 0
	sqe.addr     = uint64(uintptr(unsafe.Pointer(&buf[0])))
	sqe.len      = uint32(stat.Size)
	sqe.userData = 42

	// atualiza tail da SQ para o kernel ver a operação
	sqTail  := (*uint32)(unsafe.Pointer(&sqRaw[params.sqOff.tail]))
	sqArray := (*uint32)(unsafe.Pointer(&sqRaw[params.sqOff.array]))
	*sqArray = 0
	*sqTail  = 1

	// ── 8. Submete ao kernel e aguarda completion — 1 syscall ─────────
	// O processo não dorme aqui: io_uring_enter retorna assim que
	// o kernel colocar o resultado na CQ. Zero troca de contexto.
	if err := ioUringEnter(ringFd, 1, 1, ioringEnterGetev); err != nil {
		return fmt.Errorf("io_uring_enter: %w", err)
	}

	// ── 9. Lê resultado da CQ ─────────────────────────────────────────
	cqHead := (*uint32)(unsafe.Pointer(&cqRaw[params.cqOff.head]))
	cqTail := (*uint32)(unsafe.Pointer(&cqRaw[params.cqOff.tail]))
	cqMask := (*uint32)(unsafe.Pointer(&cqRaw[params.cqOff.ringMask]))

	if *cqHead == *cqTail {
		return fmt.Errorf("io_uring: nenhuma completion recebida")
	}

	idx := (*cqHead) & (*cqMask)
	cqeBase := uintptr(unsafe.Pointer(&cqRaw[params.cqOff.cqes]))
	cqe := (*ioUringCQE)(unsafe.Pointer(cqeBase + uintptr(idx)*unsafe.Sizeof(ioUringCQE{})))

	if cqe.res < 0 {
		return fmt.Errorf("io_uring READ falhou: errno %d", -cqe.res)
	}
	*cqHead++ // consome a CQE

	log.Printf("[io_uring] %d bytes lidos de %q — kernel sinalizou via CQE", cqe.res, path)

	// ── 10. Processa o buffer ─────────────────────────────────────────
	scanner := bufio.NewScanner(strings.NewReader(string(buf[:cqe.res])))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			books = append(books, line)
		}
	}
	log.Printf("%d livros carregados (io_uring).", len(books))
	return scanner.Err()
}

// ─────────────────────────────────────────
//  BUSCA
// ─────────────────────────────────────────
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
func onSearchRequested(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchRequested) {
		log.Printf("[evento] SEARCH_REQUESTED → busca #%d: %q", event.Payload.ID, event.Payload.Query)
		go func(e Event) {
			bus.Publish(Event{Type: EventSearchExecuting, Payload: e.Payload})
		}(event)
	}
}

func onSearchExecuting(bus *EventBus) {
	for event := range bus.Subscribe(EventSearchExecuting) {
		go func(e Event) {
			start   := time.Now()
			results := searchBooks(e.Payload.Query)
			e.Payload.Results = results
			e.Payload.Took    = time.Since(start).String()
			if len(results) == 0 {
				bus.Publish(Event{Type: EventSearchEmpty, Payload: e.Payload})
			} else {
				bus.Publish(Event{Type: EventSearchCompleted, Payload: e.Payload})
			}
		}(event)
	}
}

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
//  HTTP — handler não-bloqueante via select
// ─────────────────────────────────────────
var (
	eventBus      = NewEventBus()
	searchCounter int
	mu            sync.Mutex
)

func handleSearch(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	searchCounter++
	id := searchCounter
	mu.Unlock()

	resultChan := make(chan SearchResponse, 1)
	eventBus.Publish(Event{
		Type: EventSearchRequested,
		Payload: SearchPayload{
			ID:         id,
			Query:      r.URL.Query().Get("q"),
			ResultChan: resultChan,
		},
	})

	// select observa três canais sem bloquear o OS thread —
	// o Go runtime usa epoll internamente nos sockets/canais.
	select {
	case result := <-resultChan:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	case <-r.Context().Done():
		log.Printf("[handler] busca #%d cancelada pelo cliente", id)
		http.Error(w, "request cancelado", http.StatusRequestTimeout)
	case <-time.After(5 * time.Second):
		log.Printf("[handler] busca #%d atingiu timeout", id)
		http.Error(w, "timeout", http.StatusGatewayTimeout)
	}
}

func main() {
	if err := loadBooks("../Books.txt"); err != nil {
		log.Fatal("Erro ao carregar livros:", err)
	}

	go onSearchRequested(eventBus)
	go onSearchExecuting(eventBus)
	go onSearchCompleted(eventBus)
	go onSearchEmpty(eventBus)

	http.HandleFunc("/search", handleSearch)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK — %d livros | 4 event handlers | io_uring ativo\n", len(books))
	})

	log.Println("Servidor rodando em :8082 (io_uring + event bus)")
	log.Fatal(http.ListenAndServe(":8082", nil))
}

// Dependência:
//   go get golang.org/x/sys
//
// Como testar:
//   go run main.go
//   curl "http://localhost:8082/search?q=harry"
//   curl "http://localhost:8082/search?q=xyzabc"   ← SEARCH_EMPTY
