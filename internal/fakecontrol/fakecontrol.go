// Package fakecontrol implements an in-memory fake of the Substrate ateapi
// Control service for tests. It mimics the control plane's lifecycle
// semantics: actors are created suspended, resume/suspend/pause flip
// status, and only suspended actors can be deleted.
package fakecontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Server is a fake ateapi Control server.
type Server struct {
	ateapipb.UnimplementedControlServer

	mu     sync.Mutex
	actors map[string]*ateapipb.Actor
}

// New returns an empty fake control server.
func New() *Server {
	return &Server{actors: make(map[string]*ateapipb.Actor)}
}

// Serve starts the fake on a random localhost port and returns its
// address and a shutdown function. Like the real ateapi, it serves TLS
// with a certificate no client can verify (clients set SkipVerify).
func (s *Server) Serve() (addr string, stop func(), err error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	cert, err := selfSignedCert()
	if err != nil {
		lis.Close()
		return "", nil, err
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
	})))
	ateapipb.RegisterControlServer(grpcServer, s)
	go grpcServer.Serve(lis)
	return lis.Addr().String(), grpcServer.Stop, nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fakecontrol"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Status returns the current status of an actor, or STATUS_UNSPECIFIED if
// it does not exist.
func (s *Server) Status(id string) ateapipb.Actor_Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actors[id]
	if !ok {
		return ateapipb.Actor_STATUS_UNSPECIFIED
	}
	return a.GetStatus()
}

func (s *Server) get(id string) (*ateapipb.Actor, error) {
	a, ok := s.actors[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor %q not found", id)
	}
	return a, nil
}

func (s *Server) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActorId())
	if err != nil {
		return nil, err
	}
	return &ateapipb.GetActorResponse{Actor: proto.Clone(a).(*ateapipb.Actor)}, nil
}

func (s *Server) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := req.GetActorId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_id is required")
	}
	if _, ok := s.actors[id]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %q already exists", id)
	}
	a := &ateapipb.Actor{
		ActorId:                id,
		ActorTemplateNamespace: req.GetActorTemplateNamespace(),
		ActorTemplateName:      req.GetActorTemplateName(),
		Status:                 ateapipb.Actor_STATUS_SUSPENDED,
		WorkerSelector:         req.GetWorkerSelector(),
	}
	s.actors[id] = a
	return &ateapipb.CreateActorResponse{Actor: proto.Clone(a).(*ateapipb.Actor)}, nil
}

func (s *Server) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActorId())
	if err != nil {
		return nil, err
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	a.AteomPodName = "worker-0"
	a.AteomPodNamespace = "ate-system"
	a.AteomPodIp = "10.0.0.1"
	return &ateapipb.ResumeActorResponse{Actor: proto.Clone(a).(*ateapipb.Actor)}, nil
}

func (s *Server) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActorId())
	if err != nil {
		return nil, err
	}
	a.Status = ateapipb.Actor_STATUS_SUSPENDED
	a.AteomPodName, a.AteomPodNamespace, a.AteomPodIp = "", "", ""
	return &ateapipb.SuspendActorResponse{Actor: proto.Clone(a).(*ateapipb.Actor)}, nil
}

func (s *Server) PauseActor(ctx context.Context, req *ateapipb.PauseActorRequest) (*ateapipb.PauseActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActorId())
	if err != nil {
		return nil, err
	}
	a.Status = ateapipb.Actor_STATUS_PAUSED
	return &ateapipb.PauseActorResponse{Actor: proto.Clone(a).(*ateapipb.Actor)}, nil
}

func (s *Server) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActorId())
	if err != nil {
		return nil, err
	}
	if a.GetStatus() != ateapipb.Actor_STATUS_SUSPENDED {
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q is %s, only suspended actors can be deleted",
			req.GetActorId(), a.GetStatus())
	}
	delete(s.actors, req.GetActorId())
	return &ateapipb.DeleteActorResponse{}, nil
}

func (s *Server) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ateapipb.ListActorsResponse{}
	for _, a := range s.actors {
		resp.Actors = append(resp.Actors, proto.Clone(a).(*ateapipb.Actor))
	}
	return resp, nil
}
