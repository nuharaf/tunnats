package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
)

type config struct {
	AgentID          string `json:"agent_id"`
	Port             int    `json:"port"`
	Connect          string `json:"connect"`
	CredsPath        string `json:"creds_path"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	Keepalive        int    `json:"keepalive"`
	KeepaliveTimeout int    `json:"keepalive_timeout"`
	LogLevel         string `json:"loglevel"`
}

type napiCreateTunnelReq struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

type natsSubject struct {
	data         string
	keepalive    string
	notification string
}

type napiCreateTunnelRes struct {
	StatusCode  int    `json:"status_code"`
	SessionCode string `json:"session_code,omitempty"`
}

type apiCreateListener struct {
	RemoteHost string `json:"remote_host"`
	RemotePort string `json:"remote_port"`
	LocalHost  string `json:"local_host"`
	LocalPort  string `json:"local_port"`
}

type apiResult struct {
	StatusCode int    `json:"status"`
	Message    string `json:"message"`
}

type natsTunnel struct {
	nc *nats.Conn
}

type tunnelSession struct {
	agentID string
	session string
}
type serviceType int

var conf config

const (
	agent serviceType = iota
	client
)

func natsErrHandler(nc *nats.Conn, sub *nats.Subscription, natsErr error) {
	log.WithField("subject", sub.Subject).Errorf("NATS ERROR : %v", natsErr)
}

func main() {

	confFile, err := os.Open("config.json")
	if err != nil {
		log.Error("config file not found")
		log.Debug(err)
		return
	}
	jsonParser := json.NewDecoder(confFile)
	jsonParser.Decode(&conf)
	lvl, _ := log.ParseLevel(conf.LogLevel)
	log.SetLevel(lvl)
	var opts []nats.Option
	if conf.CredsPath == "" {
		opts = append(opts, nats.UserInfo(conf.Username, conf.Password))
	} else {
		opts = append(opts, nats.UserCredentials(conf.CredsPath))
	}
	if os.Args[1] == "agent" {
		opts = append(opts, nats.Name(fmt.Sprintf("agent_%s", conf.AgentID)))
	}
	opts = append(opts, nats.ErrorHandler(natsErrHandler))
	nc, err := nats.Connect(conf.Connect, opts...)
	if err != nil {
		log.Error("Failed to connect to nats server")
		log.Debug(err)
		return
	}
	log.Info("Successfully connected to nats server")
	tunnel := natsTunnel{nc: nc}
	if os.Args[1] == "agent" {
		_, err := tunnel.nc.Subscribe(fmt.Sprintf("create_tunnel.%s", conf.AgentID), func(msg *nats.Msg) {
			go tunnel.createTunnel(msg)
		})
		if err != nil {
			log.Error("Failed to subscribe nats api")
			log.Debug(err)
			return
		}
		for {
			time.Sleep(time.Second)
		}
	} else if os.Args[1] == "client" {
		r := mux.NewRouter()
		r.HandleFunc("/create_listener/{agent_id}", tunnel.createListener)
		err = http.ListenAndServe(net.JoinHostPort("localhost", strconv.Itoa(conf.Port)), r)
		if err != nil {
			log.WithField("port", conf.Port).Error("Failed to bind local API")
			log.Debug(err)
			return
		}
	} else {
		return
	}
}

func (nt *natsTunnel) createListener(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	agentID := vars["agent_id"]
	var tunEp apiCreateListener
	reqBody, err := ioutil.ReadAll(r.Body)
	json.Unmarshal(reqBody, &tunEp)
	listener, err := net.Listen("tcp4", net.JoinHostPort(tunEp.LocalHost, tunEp.LocalPort))
	if err != nil {
		log.WithFields(log.Fields{"host": tunEp.LocalHost, "port": tunEp.LocalPort}).Error("Failed to bind socket listener")
		log.Debug(err)
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(apiResult{StatusCode: 0, Message: err.Error()})
		return
	}
	log.WithFields(log.Fields{"host": tunEp.LocalHost, "port": tunEp.LocalPort}).Info("Listener bound")
	go nt.connAcceptor(listener, tunEp, agentID)
	json.NewEncoder(w).Encode(apiResult{StatusCode: 0})
}

func (nt *natsTunnel) connAcceptor(listener net.Listener, param apiCreateListener, agentID string) {
	for {
		conn, _ := listener.Accept()
		log.WithField("address", conn.LocalAddr().String()).Info("Accepting connection")
		nt.connHandler(conn, param, agentID)
		log.WithField("address", conn.LocalAddr().String()).Info("Closing connection")
		conn.Close()
	}
}

func (nt *natsTunnel) connHandler(conn net.Conn, param apiCreateListener, agentID string) {
	nreq, _ := json.Marshal(napiCreateTunnelReq{Host: param.RemoteHost, Port: param.RemotePort})
	msg, err := nt.nc.Request(fmt.Sprintf("create_tunnel.%s", agentID), []byte(nreq), 5*time.Second)
	if err != nil {
		log.Debug(err)
		return
	}
	var nres napiCreateTunnelRes
	json.Unmarshal(msg.Data, &nres)
	if nres.StatusCode != 0 {
		log.WithFields(log.Fields{"agent_id": agentID, "remote_host": param.RemoteHost, "remote_port": param.RemotePort}).Error("Failed to create tunnel")
		return
	}
	log.WithField("session", nres.SessionCode).Info("Receive session code")
	//run the reader
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	reader := makePipeRead(ctx, conn, nt.nc, "agent", conf.AgentID, nres.SessionCode)
	readerDone := make(chan error)
	go func() {
		err := reader()
		log.Debug(err)
		readerDone <- err
	}()

	//wait agent to be ready
	outgoing := fmt.Sprintf("socket.%s.data.%s.%s", "client", conf.AgentID, nres.SessionCode)
	for i := 1; i < 5; i++ {
		log.WithFields(log.Fields{"agent_id": conf.AgentID, "session": nres.SessionCode}).Info("Check if receiver ready")
		_, err = nt.nc.Request(outgoing, []byte{}, 1*time.Second)
		if err == nil {
			log.Info("Receiver is ready")
			break
		}
	}
	if err != nil {
		log.Error("Receiver timeout")
		return
	}
	//now agent is ready to receive
	//run the writer
	writer := makePipeWrite(conn, nt.nc, "client", conf.AgentID, nres.SessionCode)
	writerDone := make(chan error)
	go func() {
		err := writer()
		log.Debug(err)
		writerDone <- err
	}()
	for {
		select {
		case err := <-readerDone:
			if err == io.EOF {
				break
			} else if err == errPingTimeout || err == errPipeClosed {
				conn.Close()
				break
			}
		case err := <-writerDone:
			if err == io.EOF {
				nt.nc.Publish(outgoing, []byte{0})
				cancel()
				break
			}
		}
		break
	}
}

func (nt *natsTunnel) createTunnel(m *nats.Msg) {
	var dest napiCreateTunnelReq
	json.Unmarshal(m.Data, &dest)
	sessCode := strconv.Itoa(int(rand.Intn(1000000)))
	incoming := fmt.Sprintf("socket.%s.data.%s.%s", "client", conf.AgentID, sessCode)
	d := make(chan *nats.Msg, 100)
	_, err := nt.nc.ChanSubscribe(incoming, d)
	if err != nil {
		respon, _ := json.Marshal(napiCreateTunnelRes{StatusCode: -1})
		nt.nc.Publish(m.Reply, respon)
		log.Debug(err)
		return
	}
	//dial the connection
	addr := net.JoinHostPort(dest.Host, dest.Port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.WithField("address", addr).Error("Failed to dial connection")
		respon, _ := json.Marshal(napiCreateTunnelRes{StatusCode: -1})
		nt.nc.Publish(m.Reply, respon)
		return
	}
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	reader := makePipeRead(ctx, conn, nt.nc, "client", conf.AgentID, sessCode)
	readerDone := make(chan error)
	go func() {
		err := reader()
		log.Debug(err)
		readerDone <- err
	}()
	//give the session code to the client
	respon, _ := json.Marshal(napiCreateTunnelRes{StatusCode: 0, SessionCode: sessCode})
	nt.nc.Publish(m.Reply, respon)
	//wait client to be ready
	outgoing := fmt.Sprintf("socket.%s.data.%s.%s", "agent", conf.AgentID, sessCode)
	for i := 1; i < 5; i++ {
		log.WithFields(log.Fields{"agent_id": conf.AgentID, "session": sessCode}).Info("Check if receiver ready")
		_, err = nt.nc.Request(outgoing, []byte{}, 1*time.Second)
		if err == nil {
			log.Info("Receiver is ready")
			break
		}
	}
	if err != nil {
		log.Error("Receiver timeout")
		return
	}

	//now the client is ready
	writer := makePipeWrite(conn, nt.nc, "agent", conf.AgentID, sessCode)
	//run the writer
	writerDone := make(chan error)
	go func() {
		err := writer()
		log.Debug(err)
		writerDone <- err
	}()

	for {
		select {
		case err := <-readerDone:
			if err == io.EOF {
				break
			} else if err == errPingTimeout || err == errPipeClosed {
				conn.Close()
				break
			}
		case err := <-writerDone:
			if err == io.EOF {
				nt.nc.Publish(outgoing, []byte{0})
				cancel()
				break
			}
		}
		break
	}
}

func makePipeRead(ctx context.Context, conn net.Conn, nc *nats.Conn, enType, agentID, session string) func() error {
	return func() error {
		return pipeReader(ctx, conn, nc, enType, agentID, session)
	}

}

var errPingTimeout = errors.New("Ping Timeout")
var errPipeClosed = errors.New("Tunnel closed")
var errDone = errors.New("Done")

func pipeReader(ctx context.Context, conn net.Conn, nc *nats.Conn, enType, agentID, session string) error {
	d := make(chan *nats.Msg, 100)
	subject := fmt.Sprintf("socket.%s.data.%s.%s", enType, agentID, session)
	subs, err := nc.ChanSubscribe(subject, d)
	if err != nil {
		return err
	}
	defer subs.Unsubscribe()
	t := time.NewTimer(time.Duration(conf.KeepaliveTimeout) * time.Second)
	for {
		select {
		case msg := <-d:
			t.Stop()
			t.Reset(time.Duration(conf.KeepaliveTimeout) * time.Second)
			if len(msg.Data) == 0 { // treat zero byte message as ping
				log.WithFields(log.Fields{"agent_id": agentID, "session": session}).Trace("Receive ping")
				if msg.Reply != "" {
					log.WithFields(log.Fields{"agent_id": agentID, "session": session}).Trace("Replying ping")
					nc.Publish(msg.Reply, []byte{})
				}
			} else if len(msg.Data) == 1 { //treat one byte message as async notif
				log.WithFields(log.Fields{"agent_id": agentID, "session": session}).Info("Pipe closed")
				return errPipeClosed

			} else {
				d := msg.Data[1:]
				log.WithFields(log.Fields{"agent_id": agentID, "session": session, "size": len(d)}).Trace("Receiving data")
				_, err := conn.Write(d)
				if err != nil {
					return err
				}
			}
		case <-t.C:
			return errPingTimeout
		case <-ctx.Done():
			return errDone
		}

	}
}

func makePipeWrite(conn net.Conn, nc *nats.Conn, enType, agentID, session string) func() error {
	return func() error {
		return pipeWriter(conn, nc, enType, agentID, session)
	}
}

func pipeWriter(conn net.Conn, nc *nats.Conn, enType, agentID, session string) error {
	buff := make([]byte, 500)
	sub := fmt.Sprintf("socket.%s.data.%s.%s", enType, agentID, session)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Duration(conf.Keepalive) * time.Second))
		n, err := conn.Read(buff[1:])
		if n > 0 {
			log.WithFields(log.Fields{"agent_id": agentID, "session": session, "size": n}).Trace("Sending data")
			err := nc.Publish(sub, buff[:n+1])
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				log.WithFields(log.Fields{"agent_id": agentID, "session": session}).Trace("Sending ping")
				err := nc.Publish(sub, []byte{})
				if err != nil {
					return err
				}
			} else {
				return err
			}

		}

	}

}
