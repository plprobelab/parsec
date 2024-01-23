package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"

	"github.com/julienschmidt/httprouter"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"golang.org/x/sync/errgroup"

	"context"
	"errors"
	"net"

	"github.com/probe-lab/parsec/pkg/config"
	"github.com/probe-lab/parsec/pkg/db"
	"github.com/probe-lab/parsec/pkg/dht"
	"github.com/probe-lab/parsec/pkg/firehose"
	"github.com/probe-lab/parsec/pkg/models"
	"github.com/probe-lab/parsec/pkg/util"

	log "github.com/sirupsen/logrus"
)

const headerSchedulerID = "x-scheduler-id"

type Server struct {
	server   *http.Server
	done     chan struct{}
	conf     config.ServerConfig
	cancel   context.CancelFunc
	addr     string
	host     *dht.Host
	dbc      db.Client
	dbNode   *models.Node
	fhClient *firehose.Client
}

var _ network.Notifiee = (*Server)(nil)

func NewServer(ctx context.Context, dbc db.Client, conf config.ServerConfig) (*Server, error) {
	ctx, cancel := context.WithCancel(ctx)

	fhConf := &firehose.Config{
		Fleet:     conf.Fleet,
		Region:    conf.FirehoseRegion,
		Stream:    conf.FirehoseStream,
		BatchSize: conf.FirehoseBatchSize,
		BatchTime: conf.FirehoseBatchTime,
		Badbits:   conf.Badbits,
	}

	fhClient, err := firehose.NewClient(ctx, fhConf)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("new firehose client: %w", err)
	}

	parsecHost, err := dht.New(ctx, fhClient, conf)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("new host: %w", err)
	}

	fhClient.SetHost(parsecHost)

	log.Infoln("Bootstrapping DHT...")
	for _, bp := range kaddht.GetDefaultBootstrapPeerAddrInfos() {
		log.WithField("peerID", util.FmtPeerID(bp.ID)).Infoln("Connecting to bootstrap peer...")
		if err = parsecHost.Connect(ctx, bp); err != nil {
			log.WithError(err).Warnln("Could not connect to bootstrap peer")
		}
	}

	dbNode, err := dbc.InsertNode(ctx, parsecHost.ID(), conf)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("insert node: %w", err)
	}

	fhClient.SetDBNodeID(dbNode.ID)

	s := &Server{
		cancel:   cancel,
		dbc:      dbc,
		conf:     conf,
		addr:     fmt.Sprintf("%s:%d", conf.ServerHost, conf.ServerPort),
		host:     parsecHost,
		dbNode:   dbNode,
		fhClient: fhClient,
		done:     make(chan struct{}),
	}

	if conf.FirehoseConnectionEvents {
		parsecHost.Network().Notify(s)
	}

	return s, nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	defer func() {
		log.Infoln("Updating server offline timestamp...")
		if err := s.dbc.UpdateOfflineSince(ctx, s.dbNode); err != nil {
			log.WithError(err).WithField("nodeID", s.dbNode.ID).Warnln("Couldn't update offline since field of node")
		}
	}()

	s.host.Network().StopNotify(s)

	errg := errgroup.Group{}

	errg.Go(func() error {
		if s.server == nil {
			return nil
		}
		log.Infoln("Stopping server...")
		return s.server.Shutdown(ctx)
	})
	errg.Go(func() error {
		log.Infoln("Stopping p2p host...")
		return s.host.Close()
	})
	s.cancel()

	if err := errg.Wait(); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) ListenAddr() string {
	return s.addr
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	tcpListener, err := net.Listen("tcp", s.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen tcp: %w", err)
	}

	go func() {
		// Start by waiting three minutes until the node is ready.
		time.Sleep(s.conf.StartupDelay)

		if err := s.dbc.UpdateHeartbeat(ctx, s.dbNode); err != nil {
			log.WithError(err).Warnln("Couldn't update heartbeat")
		}

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
			case <-s.done:
				return
			case <-ctx.Done():
				return
			}

			if err := s.dbc.UpdateHeartbeat(ctx, s.dbNode); err != nil {
				log.WithError(err).Warnln("Couldn't update heartbeat")
			}
		}
	}()

	router := httprouter.New()
	router.POST("/provide", s.provide)
	router.POST("/retrieve/:cid", s.retrieve)
	router.GET("/readiness", s.readiness)

	s.server = &http.Server{
		Handler:     s.metricsHandler(s.logHandler(router)),
		BaseContext: func(listener net.Listener) context.Context { return ctx },
	}

	defer func() {
		close(s.done)
	}()

	err = s.server.Serve(tcpListener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

func (s *Server) logHandler(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		log.WithFields(log.Fields{
			"url":    r.URL.String(),
			"method": r.Method,
		}).Infoln("Received Request")

		h.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (s *Server) metricsHandler(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")

		path := "-"
		if len(parts) > 1 {
			path = parts[1]
		}

		totalRequests.WithLabelValues(r.Method, path, r.Header.Get(headerSchedulerID)).Inc()

		h.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
