package grpc_server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/Litekube/network-controller/certs"
	"github.com/Litekube/network-controller/config"
	"github.com/Litekube/network-controller/contant"
	"github.com/Litekube/network-controller/grpc/pb_gen"
	"github.com/Litekube/network-controller/internal"
	"github.com/Litekube/network-controller/utils"
	"github.com/op/go-logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"io/ioutil"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

type GrpcServer struct {
	*pb_gen.UnimplementedLiteKubeNCServiceServer
	*pb_gen.UnimplementedLiteKubeNCBootstrapServiceServer
	ctx               context.Context
	stopCh            chan struct{}
	port              int
	bootstrapPort     int
	networkServerPort int
	UnRegisterCh      chan string
	service           *internal.NetworkControllerService
	grpcTlsConfig     config.TLSConfig
	networkTlsConfig  config.TLSConfig
	tlsDir            string
}

var logger *logging.Logger
var gServer *GrpcServer

func GetGServer() *GrpcServer {
	return gServer
}

func NewGrpcServer(cfg config.ServerConfig, ctx context.Context, stopCh chan struct{}, Logger *logging.Logger, unRegisterCh chan string, serverIp string) *GrpcServer {
	s := &GrpcServer{
		ctx:               ctx,
		stopCh:            stopCh,
		port:              cfg.GrpcPort,
		bootstrapPort:     cfg.BootstrapPort,
		networkServerPort: cfg.Port,
		UnRegisterCh:      unRegisterCh,
		tlsDir:            cfg.TlsDir,
		grpcTlsConfig: config.TLSConfig{
			CAFile:         cfg.GrpcCAFile,
			CAKeyFile:      cfg.GrpcCAKeyFile,
			ServerCertFile: cfg.GrpcServerCertFile,
			ServerKeyFile:  cfg.GrpcServerKeyFile,
			ClientCertFile: filepath.Join(cfg.GrpcCertDir, contant.ClientCertFile),
			ClientKeyFile:  filepath.Join(cfg.GrpcCertDir, contant.ClientKeyFile),
		},
		networkTlsConfig: config.TLSConfig{
			CAFile:         cfg.NetworkCAFile,
			CAKeyFile:      cfg.NetworkCAKeyFile,
			ServerCertFile: cfg.NetworkServerCertFile,
			ServerKeyFile:  cfg.NetworkServerKeyFile,
			ClientCertFile: filepath.Join(cfg.NetworkCertDir, contant.ClientCertFile),
			ClientKeyFile:  filepath.Join(cfg.NetworkCertDir, contant.ClientKeyFile),
		},
	}

	logger = Logger

	ip := cfg.Ip
	if ip == "" {
		//backup
		ip = utils.QueryPublicIp()
	}

	s.service = internal.NewLiteNCService(Logger, unRegisterCh, s.grpcTlsConfig, s.networkTlsConfig, ip, serverIp, strconv.Itoa(cfg.BootstrapPort), strconv.Itoa(cfg.GrpcPort), strconv.Itoa(cfg.Port))
	return s
}

//func StartGrpcServer(cfg config.ServerConfig, unRegisterCh chan string) {
//	//utils.CreateDir(cfg.GrpcCertDir)
//	//err := certs.CheckGrpcCertConfig(gServer.grpcTlsConfig)
//	//if err != nil {
//	//	logger.Error(err)
//	//}
//	go gServer.StartGrpcServerTcp()
//	go gServer.StartBootstrapServerTcp()
//}

func (s *GrpcServer) StartGrpcServerTcp() error {
	defer logger.Debug("StartGrpcServerTcp done")

	err := certs.CheckGrpcClientCertConfig(s.grpcTlsConfig, s.tlsDir)
	if err != nil {
		logger.Errorf("CheckGrpcClientCertConfig err: %+v", err)
		return err
	}
	tcpAddr := fmt.Sprintf(":%d", s.port)
	lis, err := net.Listen("tcp", tcpAddr)
	defer lis.Close()
	if err != nil {
		logger.Errorf("tcp failed to listen: %v", err)
		return err
	}

	gopts := []grpc.ServerOption{}
	if len(s.grpcTlsConfig.ServerCertFile) != 0 && len(s.grpcTlsConfig.ServerKeyFile) != 0 {
		creds, err := credentials.NewServerTLSFromFile(s.grpcTlsConfig.ServerCertFile, s.grpcTlsConfig.ServerKeyFile)
		if err != nil {
			logger.Error(err)
			return err
		}
		gopts = append(gopts, grpc.Creds(creds))
	}
	cert, err := tls.LoadX509KeyPair(s.grpcTlsConfig.ServerCertFile, s.grpcTlsConfig.ServerKeyFile)
	//cert, err := certificate.LoadCertificate(s.CertFile)
	if err != nil {
		logger.Errorf("tls.LoadX509KeyPair err: %v", err)
	}

	certPool := x509.NewCertPool()
	ca, err := ioutil.ReadFile(s.grpcTlsConfig.CAFile)
	if err != nil {
		logger.Errorf("ioutil.ReadFile err: %v", err)
	}

	if ok := certPool.AppendCertsFromPEM(ca); !ok {
		logger.Errorf("certPool.AppendCertsFromPEM err")
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		// fix here
		ClientAuth: tls.RequireAndVerifyClientCert,
		//ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs: certPool,
	})

	//interceptor := grpc.UnaryInterceptor(TokenInterceptor)
	//gopts = append(gopts, []grpc.ServerOption{grpc.Creds(creds), interceptor}...)

	gopts = append(gopts, []grpc.ServerOption{grpc.Creds(creds)}...)
	server := grpc.NewServer(gopts...)
	// register reflection for grpcurl service
	reflection.Register(server)
	// register service
	pb_gen.RegisterLiteKubeNCServiceServer(server, s)
	logger.Infof("grpc server ready to serve at %+v", tcpAddr)

	go func() {
		for {
			select {
			case <-s.stopCh:
				server.GracefulStop()
				return
			}
		}
	}()

	if err := server.Serve(lis); err != nil {
		logger.Errorf("grpc server failed to serve: %v", err)
		return err
	}
	return nil
}

func TokenInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {

	//通过metadata
	md, exist := metadata.FromIncomingContext(ctx)
	if !exist {
		return nil, status.Errorf(codes.Unauthenticated, "no metadata")
	}

	if strings.Contains(info.FullMethod, "/GetToken") {
		// bootstrap, handle directly
		// check bootstrap token
		if _, ok := md["bootstrap-token"]; !ok {
			return nil, status.Errorf(codes.Aborted, "plz provide bootstrap-token")
		}
	} else {
		// check token
		if _, ok := md["node-token"]; !ok {
			return nil, status.Errorf(codes.Aborted, "plz provide node-token")
		}
	}
	return handler(ctx, req)
}
