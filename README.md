# Raft Consensus Visualizer

A from-scratch implementation of the [Raft](https://raft.github.io/raft.pdf) distributed consensus algorithm in Go — with a live dashboard to watch leader elections, log replication, and fault recovery happen in real time.

<br/>

<div align="center">
  <video src="https://github.com/user-attachments/assets/74b16ebe-3a12-4acb-b604-aa757578adc0" autoplay loop muted playsinline width="90%"></video>
</div>

<br/>

## What is Raft?

Raft is a consensus algorithm that keeps a cluster of distributed nodes in agreement on a shared log — even when nodes crash, restart, or fall behind. It's the backbone of systems like **etcd**, **CockroachDB**, and **TiKV**.

This project simulates a **5-node Raft cluster** entirely in Go using goroutines as nodes and channels as the network. No external consensus library — built directly from the paper.

<br/>

## Try it yourself

```bash
git clone https://github.com/krivansemlani/raft-consensus-visualizer.git
cd raft-consensus-visualizer
go run main.go
```

Open **http://localhost:8081**

<br/>

## What you can do

| Action | How |
|---|---|
| **Kill a node** | Click **×** on any node row, or click the node in the graph |
| **Revive a node** | Click **↑** (appears when node is dead) |
| **Send a command** | Preset buttons or custom input → Enter |
| **Watch replication** | Log panel — 🟡 gold = uncommitted, 🟢 green = committed |

**Scenario to try:** Kill the leader. Watch the remaining nodes detect the timeout, campaign for election, and elect a new leader — all within ~300ms. Revive the dead node and watch it catch up automatically.

<br/>

## What's implemented from the Raft paper

- [x] Leader election with randomized timeouts (§5.2)
- [x] Log replication via `AppendEntries` RPC (§5.3)
- [x] `prevLogIndex` / `prevLogTerm` consistency check (§5.3)
- [x] Leader backoff — `NextIndex` decrement on reject, retry until match
- [x] Majority commit — `MatchIndex` tracking across followers (§5.3)
- [x] Safety — leader only commits entries from its own term (§5.4)
- [x] Full log catch-up after node revival

<br/>

## Architecture

```
main.go
├── Node{}               per-node state: term, log, votedFor, nextIndex, matchIndex
├── run()                message loop — one goroutine per node
├── startElectionTimer() randomized 150–300ms timeout, restarts on heartbeat
├── becomeLeader()       init NextIndex/MatchIndex, launch 50ms heartbeat loop
├── AppendEntries        prevLog check → truncate conflicts → append → ack
├── RequestVote          step down on higher term, grant vote if votedFor is clear
└── HTTP handlers        /ws · /kill/:id · /revive/:id · /command/:cmd
```

The "network" is pure Go channels — each node has an `Inbox chan Message`. No sockets between nodes, no Protobuf — just goroutines writing to each other's inboxes. The HTTP layer is only for the browser dashboard.

<br/>

## Stack

- **Go** — goroutines, channels, `net/http`, `gorilla/websocket`, `//go:embed`
- **Vanilla JS** — WebSocket, SVG animations, in-place DOM updates (no framework)
- **Fonts** — DM Serif Display · DM Mono · DM Sans
