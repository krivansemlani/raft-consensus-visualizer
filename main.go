package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed raft-dashboard.html
var dashboardHTML []byte

// LogEntry is used only in WebSocket JSON responses.
type LogEntry struct {
	Index   int    `json:"index"`
	Term    int    `json:"term"`
	Command string `json:"command"`
}

type NodeState struct {
	ID          int        `json:"id"`
	State       string     `json:"state"`
	CurrentTerm int        `json:"currentTerm"`
	Dead        bool       `json:"dead"`
	LogLength   int        `json:"logLength"`
	CommitIndex int        `json:"commitIndex"`
	Log         []LogEntry `json:"log"`
}

type Entry struct {
	Term    int
	Command string
}

type Message struct {
	Type         string
	Term         int
	From         int
	Entries      []Entry
	PrevLogIndex int // index of log entry immediately preceding new ones
	PrevLogTerm  int // term of PrevLogIndex entry
	LeaderCommit int // leader's commitIndex
	AckIndex     int // highest log index acked (used in AckEntries)
}

type Node struct {
	ID            int
	State         string // "follower", "candidate", "leader", "dead"
	CurrentTerm   int
	VotedFor      int
	Log           []Entry
	Votes         int
	LastHeartbeat time.Time
	CommitIndex   int
	Dead          bool
	NextIndex     map[int]int // leader only: next log index to send to each follower
	MatchIndex    map[int]int // leader only: highest log index known replicated on each follower
	Inbox         chan Message
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func wsHandler(nodes []*Node) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			state := make([]NodeState, len(nodes))
			for i, n := range nodes {
				logEntries := make([]LogEntry, len(n.Log))
				for j, e := range n.Log {
					logEntries[j] = LogEntry{Index: j, Term: e.Term, Command: e.Command}
				}
				state[i] = NodeState{
					ID:          n.ID,
					State:       n.State,
					CurrentTerm: n.CurrentTerm,
					Dead:        n.Dead,
					LogLength:   len(n.Log),
					CommitIndex: n.CommitIndex,
					Log:         logEntries,
				}
			}
			data, err := json.Marshal(state)
			if err != nil {
				return
			}
			if err = conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func killHandler(nodes []*Node) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/kill/"))
		if err != nil || id < 0 || id >= len(nodes) {
			http.Error(w, "invalid node id", http.StatusBadRequest)
			return
		}
		nodes[id].Dead = true
		nodes[id].State = "dead"
		fmt.Printf("Node %d killed\n", id)
		w.WriteHeader(http.StatusOK)
	}
}

func reviveHandler(nodes []*Node) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/revive/"))
		if err != nil || id < 0 || id >= len(nodes) {
			http.Error(w, "invalid node id", http.StatusBadRequest)
			return
		}
		nodes[id].Dead = false
		nodes[id].State = "follower"
		nodes[id].VotedFor = -1
		// Set LastHeartbeat to now so the node waits a full timeout before
		// triggering an election, giving the existing leader time to reach it.
		nodes[id].LastHeartbeat = time.Now()
		fmt.Printf("Node %d revived\n", id)
		w.WriteHeader(http.StatusOK)
	}
}

func commandHandler(nodes []*Node) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		command := strings.TrimPrefix(r.URL.Path, "/command/")
		for _, node := range nodes {
			if node.State == "leader" && !node.Dead {
				node.receiveCommand(command)
				fmt.Fprintf(w, "command sent to leader Node %d", node.ID)
				return
			}
		}
		http.Error(w, "no leader found", http.StatusServiceUnavailable)
	}
}

func (n *Node) run(nodes []*Node) {
	go n.startElectionTimer(nodes)
	for msg := range n.Inbox {
		if n.Dead {
			continue
		}
		switch msg.Type {

		case "RequestVote":
			// If we see a higher term, step down and reset vote so we can vote in the new term.
			if msg.Term > n.CurrentTerm {
				n.CurrentTerm = msg.Term
				n.VotedFor = -1
				n.State = "follower"
			}
			if n.VotedFor == -1 && msg.Term >= n.CurrentTerm {
				n.VotedFor = msg.From
				select {
				case nodes[msg.From].Inbox <- Message{Type: "VoteGranted", Term: n.CurrentTerm, From: n.ID}:
				default:
				}
			}

		case "VoteGranted":
			// Only count votes for the current term while still a candidate.
			if msg.Term == n.CurrentTerm && n.State == "candidate" {
				n.Votes++
				if n.Votes >= (len(nodes)/2)+1 {
					n.becomeLeader(nodes)
				}
			}

		case "AppendEntries":
			if msg.Term < n.CurrentTerm {
				break
			}
			n.State = "follower"
			n.VotedFor = -1
			n.CurrentTerm = msg.Term
			n.LastHeartbeat = time.Now()

			// Always check prevLog consistency — even for empty heartbeats.
			// This is what lets a lagging follower (e.g. after revive) signal
			// the leader to back up NextIndex so it can send the missing entries.
			if msg.PrevLogIndex >= 0 {
				if msg.PrevLogIndex >= len(n.Log) || n.Log[msg.PrevLogIndex].Term != msg.PrevLogTerm {
					select {
					case nodes[msg.From].Inbox <- Message{Type: "NackEntries", Term: n.CurrentTerm, From: n.ID}:
					default:
					}
					break
				}
			}

			if len(msg.Entries) > 0 {
				// Truncate any conflicting suffix and append new entries.
				n.Log = append(n.Log[:msg.PrevLogIndex+1], msg.Entries...)
				ackIdx := len(n.Log) - 1
				fmt.Printf("Node %d appended entries, log length now %d\n", n.ID, len(n.Log))
				select {
				case nodes[msg.From].Inbox <- Message{Type: "AckEntries", Term: n.CurrentTerm, From: n.ID, AckIndex: ackIdx}:
				default:
				}
			}

			// Advance commit index based on what the leader says is committed.
			if msg.LeaderCommit > n.CommitIndex && len(n.Log) > 0 {
				newCommit := msg.LeaderCommit
				if newCommit >= len(n.Log) {
					newCommit = len(n.Log) - 1
				}
				if newCommit > n.CommitIndex {
					n.CommitIndex = newCommit
					fmt.Printf("Node %d committed up to index %d\n", n.ID, n.CommitIndex)
				}
			}

		case "NackEntries":
			// Follower's log is behind — back up NextIndex and retry on next heartbeat.
			if n.State == "leader" && n.NextIndex[msg.From] > 0 {
				n.NextIndex[msg.From]--
			}

		case "AckEntries":
			if n.State != "leader" {
				break
			}
			// Advance what we know the follower has.
			n.NextIndex[msg.From] = msg.AckIndex + 1
			n.MatchIndex[msg.From] = msg.AckIndex

			// Find the highest N > commitIndex such that a majority of nodes
			// have matchIndex >= N and log[N].term == currentTerm (Raft §5.3/5.4).
			majority := (len(nodes) / 2) + 1
			for N := len(n.Log) - 1; N > n.CommitIndex; N-- {
				if N >= len(n.Log) || n.Log[N].Term != n.CurrentTerm {
					continue
				}
				count := 1 // leader has it
				for _, peer := range nodes {
					if peer.ID != n.ID && n.MatchIndex[peer.ID] >= N {
						count++
					}
				}
				if count >= majority {
					n.CommitIndex = N
					fmt.Printf("Leader Node %d committed entry %d\n", n.ID, N)
					break
				}
			}
		}
	}
}

func (n *Node) becomeLeader(nodes []*Node) {
	n.State = "leader"
	fmt.Printf("Node %d is LEADER for term %d\n", n.ID, n.CurrentTerm)

	// Reinitialize leader volatile state.
	n.NextIndex = make(map[int]int)
	n.MatchIndex = make(map[int]int)
	for _, peer := range nodes {
		if peer.ID != n.ID {
			n.NextIndex[peer.ID] = len(n.Log) // optimistic: start by sending nothing
			n.MatchIndex[peer.ID] = -1
		}
	}

	go func() {
		for {
			if n.Dead || n.State != "leader" {
				return
			}
			for _, peer := range nodes {
				if peer.ID == n.ID || peer.Dead {
					continue
				}
				nextIdx := n.NextIndex[peer.ID]
				if nextIdx < 0 {
					nextIdx = 0
				}
				if nextIdx > len(n.Log) {
					nextIdx = len(n.Log)
				}
				prevLogIndex := nextIdx - 1
				prevLogTerm := 0
				if prevLogIndex >= 0 && prevLogIndex < len(n.Log) {
					prevLogTerm = n.Log[prevLogIndex].Term
				}
				// Send only entries the follower doesn't have yet.
				entries := make([]Entry, len(n.Log[nextIdx:]))
				copy(entries, n.Log[nextIdx:])

				select {
				case peer.Inbox <- Message{
					Type:         "AppendEntries",
					Term:         n.CurrentTerm,
					From:         n.ID,
					Entries:      entries,
					PrevLogIndex: prevLogIndex,
					PrevLogTerm:  prevLogTerm,
					LeaderCommit: n.CommitIndex,
				}:
				default: // inbox full, skip this tick
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

func (n *Node) receiveCommand(command string) {
	entry := Entry{Term: n.CurrentTerm, Command: command}
	n.Log = append(n.Log, entry)
	fmt.Printf("Leader Node %d accepted command '%s' at log index %d\n", n.ID, command, len(n.Log)-1)
	// The heartbeat loop will replicate this to followers on the next tick.
}

func (n *Node) startElection(nodes []*Node) {
	n.State = "candidate"
	n.CurrentTerm++
	n.Votes = 1
	n.VotedFor = n.ID
	fmt.Printf("Node %d starting election for term %d\n", n.ID, n.CurrentTerm)
	for _, peer := range nodes {
		if peer.ID != n.ID {
			select {
			case peer.Inbox <- Message{Type: "RequestVote", Term: n.CurrentTerm, From: n.ID}:
			default:
			}
		}
	}
}

func (n *Node) startElectionTimer(nodes []*Node) {
	for {
		timeout := time.Duration(150+rand.Intn(150)) * time.Millisecond
		time.Sleep(timeout)
		if n.Dead {
			continue
		}
		if (n.State == "follower" || n.State == "candidate") && time.Since(n.LastHeartbeat) > timeout {
			n.startElection(nodes)
		}
	}
}

func main() {
	nodes := make([]*Node, 5)
	for i := 0; i < 5; i++ {
		nodes[i] = &Node{
			ID:         i,
			State:      "follower",
			Inbox:      make(chan Message, 100),
			VotedFor:   -1,
			MatchIndex: make(map[int]int),
			NextIndex:  make(map[int]int),
		}
		fmt.Printf("Created node %d\n", nodes[i].ID)
	}

	for _, node := range nodes {
		go node.run(nodes)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	http.HandleFunc("/ws", wsHandler(nodes))
	http.HandleFunc("/kill/", corsMiddleware(killHandler(nodes)))
	http.HandleFunc("/revive/", corsMiddleware(reviveHandler(nodes)))
	http.HandleFunc("/command/", corsMiddleware(commandHandler(nodes)))

	fmt.Println("Raft server running on :8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
