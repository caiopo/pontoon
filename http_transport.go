package pontoon

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type HTTPTransport struct {
	Address  string
	node     *Node
	listener net.Listener
}

func (t *HTTPTransport) Serve(node *Node) error {
	t.node = node

	httpListener, err := net.Listen("tcp", t.Address)
	if err != nil {
		log.Fatalf("FATAL: listen (%s) failed - %s", t.Address, err)
	}
	t.listener = httpListener

	go func() {
		log.Printf("[%s] starting HTTP server", t.node.ID)
		server := &http.Server{
			Handler: t,
		}
		err := server.Serve(httpListener)
		// theres no direct way to detect this error because it is not exposed
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("ERROR: http.Serve() - %s", err)
		}
		close(t.node.exitChan)
		log.Printf("[%s] exiting Serve()", t.node.ID)
	}()

	return nil
}

func (t *HTTPTransport) Close() error {
	return t.listener.Close()
}

func (t *HTTPTransport) String() string {
	return t.listener.Addr().String()
}

func (t *HTTPTransport) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// if req.Method != "POST" {
	// 	apiResponse(w, 405, "MUST USE HTTP POST")
	// }

	switch req.URL.Path {
	case "/ping":
		t.pingHandler(w, req)
	case "/request_vote":
		t.requestVoteHandler(w, req)
	case "/append_entries":
		t.appendEntriesHandler(w, req)
	case "/command":
		t.commandHandler(w, req)
	case "/print":
		fmt.Fprint(w, t.node.Log.PrintAll())
	case "/cluster":
		var str string
		for _, peer := range t.node.Cluster {
			str += peer.ID + " "
		}
		apiResponse(w, 299, str)

	case "/node":
		apiResponse(w, 299, strconv.Itoa(t.node.State))

	default:

		if t.node.State != Leader {
			command := strings.Split(req.URL.Path, "/")

			if command[1] == "test" && len(command) > 3 {

				respChan := make(chan CommandResponse, 1)

				id, _ := strconv.Atoi(command[2])

				body := []byte(command[3])

				cr := CommandRequest{
					ID:           int64(id),
					Name:         "SUP",
					Body:         body,
					ResponseChan: respChan,
				}

				t.node.Command(cr)

				if (<-respChan).Success {
					apiResponse(w, 299, "Sucesso!")
				} else {
					apiResponse(w, 403, "Erro :(")
				}
			}
		} else {

			http.Redirect(w, req, findLeaderIP(t.node)+req.URL.Path, 301)

		}

	}
}

func apiResponse(w http.ResponseWriter, statusCode int, data interface{}) {
	response, err := json.Marshal(data)
	if err != nil {
		response = []byte("INTERNAL_ERROR")
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(response)))
	w.WriteHeader(statusCode)
	w.Write(response)
}

func (t *HTTPTransport) pingHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Length", "2")
	io.WriteString(w, "OK")
}

func (t *HTTPTransport) requestVoteHandler(w http.ResponseWriter, req *http.Request) {
	var vr VoteRequest

	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		apiResponse(w, 500, nil)
		return
	}

	err = json.Unmarshal(data, &vr)
	if err != nil {
		apiResponse(w, 500, nil)
		return
	}

	resp, err := t.node.RequestVote(vr)
	if err != nil {
		apiResponse(w, 500, nil)
		return
	}

	apiResponse(w, 200, resp)
}

func (t *HTTPTransport) appendEntriesHandler(w http.ResponseWriter, req *http.Request) {
	var er EntryRequest

	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		apiResponse(w, 500, nil)
		return
	}

	err = json.Unmarshal(data, &er)
	if err != nil {
		apiResponse(w, 500, nil)
		return
	}

	resp, err := t.node.AppendEntries(er)
	if err != nil {
		apiResponse(w, 500, nil)
	}

	apiResponse(w, 200, resp)
}

// TODO: split this out into peer transport and client transport
// move into client transport
func (t *HTTPTransport) commandHandler(w http.ResponseWriter, req *http.Request) {
	var cr CommandRequest

	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		apiResponse(w, 500, "Sua requisicao esta vazia")
		return
	}

	err = json.Unmarshal(data, &cr)
	if err != nil {
		apiResponse(w, 500, "Unmarshall error")
		return
	}

	cr.ResponseChan = make(chan CommandResponse, 1)
	t.node.Command(cr)
	resp := <-cr.ResponseChan
	if !resp.Success {
		apiResponse(w, 500, "Comando nao foi bem sucedido")
	}

	apiResponse(w, 200, resp)
}

func (t *HTTPTransport) RequestVoteRPC(address string, voteRequest VoteRequest) (VoteResponse, error) {
	endpoint := fmt.Sprintf("http://%s/request_vote", address)
	log.Printf("[%s] RequestVoteRPC %+v to %s", t.node.ID, voteRequest, endpoint)
	data, err := apiRequest("POST", endpoint, voteRequest, 100*time.Millisecond)
	if err != nil {
		return VoteResponse{}, err
	}
	term, _ := data.Get("term").Int64()
	voteGranted, _ := data.Get("vote_granted").Bool()
	vresp := VoteResponse{
		Term:        term,
		VoteGranted: voteGranted,
	}
	return vresp, nil
}

func (t *HTTPTransport) AppendEntriesRPC(address string, entryRequest EntryRequest) (EntryResponse, error) {
	endpoint := fmt.Sprintf("http://%s/append_entries", address)
	log.Printf("[%s] AppendEntriesRPC %+v to %s", t.node.ID, entryRequest, endpoint)
	_, err := apiRequest("POST", endpoint, entryRequest, 500*time.Millisecond)
	if err != nil {
		return EntryResponse{}, err
	}
	return EntryResponse{}, nil
}

func findLeaderIP(node *Node) (ip string) {

	ipchan := make(chan string)

	for _, n := range node.Cluster {
		go func(ip string) {
			resp, err := http.Get(ip + PORT + "/node")
			if err == nil {
				defer resp.Body.Close()

				body, err := ioutil.ReadAll(resp.Body)
				if err == nil {
					ss := string(body[:])

					fmt.Println(ss)

					if ss[1] == byte('2') {
						ipchan <- ip
					}
				}
			}
		}(n.ID)
	}

	return <-ipchan
}
