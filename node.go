package pontoon

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	Follower = iota
	Candidate
	Leader
)

type Node struct {
	sync.RWMutex

	Address  string
	State    int
	Term     int64
	VotedFor string
	Log      *Log
	Votes    int
	Cluster  []string

	httpListener net.Listener

	exitChan          chan int
	requestVoteChan   chan VoteRequest
	voteResponseChan  chan int
	appendEntriesChan chan EntryRequest
	termDiscoverChan  chan int64

	endElectionChan      chan int
	finishedElectionChan chan int
}

func NewNode(address string) *Node {
	node := &Node{
		Address:           address,
		Log:               &Log{},
		exitChan:          make(chan int),
		requestVoteChan:   make(chan VoteRequest),
		voteResponseChan:  make(chan int),
		appendEntriesChan: make(chan EntryRequest),
		termDiscoverChan:  make(chan int64),
	}
	go node.StateMachine()
	return node
}

func (n *Node) Exit() error {
	return n.httpListener.Close()
}

func (n *Node) AddToCluster(member string) {
	n.Cluster = append(n.Cluster, member)
}

func (n *Node) Serve() {
	httpListener, err := net.Listen("tcp", n.Address)
	if err != nil {
		log.Fatalf("FATAL: listen (%s) failed - %s", n.Address, err.Error())
	}

	server := &http.Server{
		Handler: n,
	}
	err = server.Serve(httpListener)
	// theres no direct way to detect this error because it is not exposed
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		log.Printf("ERROR: http.Serve() - %s", err.Error())
	}

	close(n.exitChan)
	log.Printf("exiting Serve()")
}

func (n *Node) StateMachine() {
	electionTimeout := 500 * time.Millisecond
	electionTimer := time.NewTimer(electionTimeout)
	heartbeatInterval := 100 * time.Millisecond
	heartbeatTimer := time.NewTicker(heartbeatInterval)
	for {
		select {
		case <-n.exitChan:
			goto exit
		case newTerm := <-n.termDiscoverChan:
			if n.endElectionChan != nil {
				// we discovered a new term in the current election, end it
				n.EndElection()
			}
			n.SetTerm(newTerm)
		case vreq := <-n.requestVoteChan:
			_ = vreq
		case ae := <-n.appendEntriesChan:
			// TODO: check if we're a candidate and end the election (someone else became leader)
			_ = ae
		case <-electionTimer.C:
			if n.endElectionChan != nil {
				// the current election timed out, end it
				n.EndElection()
			}
			n.NextTerm()
			n.RunForLeader()
		case <-n.voteResponseChan:
			n.Lock()
			n.Votes++
			votes := n.Votes
			majority := (len(n.Cluster)/2 + 1)
			n.Unlock()
			if votes >= majority {
				// we won election, end it and promote
				n.EndElection()
				n.PromoteToLeader()
			}
		case <-heartbeatTimer.C:
			n.RLock()
			state := n.State
			n.RUnlock()
			if state == Leader {
				// TODO: send heartbeats
			}
		}

		if !electionTimer.Reset(electionTimeout) {
			electionTimer = time.NewTimer(electionTimeout)
		}
	}

exit:
	log.Printf("exiting StateMachine()")
}

func (n *Node) SetTerm(term int64) {
	n.Lock()
	defer n.Unlock()
	n.Term = term
	n.State = Follower
	n.VotedFor = ""
	n.Votes = 0
}

func (n *Node) NextTerm() {
	n.Lock()
	defer n.Unlock()
	n.Term++
	n.State = Follower
	n.VotedFor = ""
	n.Votes = 0
}

func (n *Node) PromoteToLeader() {
	n.Lock()
	defer n.Unlock()
	n.State = Leader
}

func (n *Node) EndElection() {
	close(n.endElectionChan)
	n.endElectionChan = nil
	<-n.finishedElectionChan
	n.finishedElectionChan = nil
}

// - Increment currentTerm, vote for self
// - Reset election timeout
// - Send RequestVote RPCs to all other servers, wait for either:
//   - Votes received from majority of servers: become leader
//   - AppendEntries RPC received from new leader: step down
//   - Election timeout elapses without election resolution: increment term, start new election
//   - Discover higher term: step down
func (n *Node) RunForLeader() {
	n.Lock()
	n.State = Candidate
	n.Votes++
	electionTerm := n.Term
	n.endElectionChan = make(chan int)
	n.finishedElectionChan = make(chan int)
	n.Unlock()

	go func() {
		for {
			voteResponseChan := make(chan *VoteResponse, len(n.Cluster))
			for _, peer := range n.Cluster {
				go func(p string) {
					voteResponseChan <- n.SendRequestVote(p)
				}(peer)
			}

			for {
				select {
				case resp := <-voteResponseChan:
					if resp == nil {
						// TODO: should be retrying these
						continue
					}
					if resp.Term > electionTerm {
						// we discovered a higher term
						n.termDiscoverChan <- resp.Term
						continue
					}
					if resp.VoteGranted {
						n.voteResponseChan <- 1
					}
				case <-n.endElectionChan:
					close(n.finishedElectionChan)
					return
				}
			}
		}
	}()
}

func (n *Node) SendRequestVote(peer string) *VoteResponse {
	endpoint := fmt.Sprintf("http://%s/request_vote", peer)
	log.Printf("querying %s", endpoint)
	vr := VoteRequest{
		Term:         n.Term,
		CandidateID:  n.Address,
		LastLogIndex: n.Log.Index,
		LastLogTerm:  n.Log.Term,
	}
	data, err := ApiRequest(endpoint, vr, 100*time.Millisecond)
	if err != nil {
		log.Printf("ERROR: %s - %s", endpoint, err.Error())
		return nil
	}
	term, _ := data.Get("term").Int64()
	voteGranted, _ := data.Get("vote_granted").Bool()
	return &VoteResponse{
		Term: term,
		VoteGranted: voteGranted,
	}
}

func (n *Node) RequestVote(vr VoteRequest) (VoteResponse, error) {
	if vr.Term < n.Term {
		return VoteResponse{n.Term, false}, nil
	}

	if vr.Term > n.Term {
		n.termDiscoverChan <- vr.Term
		return VoteResponse{n.Term, false}, nil
	}

	if n.VotedFor != "" && n.VotedFor != vr.CandidateID {
		return VoteResponse{n.Term, false}, nil
	}

	// TODO: check log
	return VoteResponse{n.Term, true}, nil
}

func (n *Node) AppendEntries(e EntryRequest) (EntryResponse, error) {
	return EntryResponse{}, nil
}