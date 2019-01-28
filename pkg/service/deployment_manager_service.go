/*
 *  Copyright (C) 2018 Nalej Group - All Rights Reserved
 *
 *
 */

package service

import (
    "crypto/tls"
    "fmt"
    "github.com/nalej/deployment-manager/internal/structures/monitor"
    "github.com/nalej/deployment-manager/pkg/handler"
    "github.com/nalej/deployment-manager/pkg/kubernetes"
    "github.com/nalej/deployment-manager/pkg/login-helper"
    monitor2 "github.com/nalej/deployment-manager/pkg/monitor"
    "github.com/nalej/deployment-manager/pkg/network"
    "github.com/nalej/deployment-manager/pkg/utils"
    "github.com/nalej/derrors"
    pbDeploymentMgr "github.com/nalej/grpc-deployment-manager-go"
    "github.com/nalej/grpc-utils/pkg/tools"
    "github.com/rs/zerolog/log"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
    "google.golang.org/grpc/reflection"
    "net"
    "os"
    "strconv"
    "strings"
    "github.com/nalej/deployment-manager/pkg/common"
)

type DeploymentManagerService struct {
    // Manager with the logic for incoming requests
    mgr *handler.Manager
    // Manager for networking services
    net *network.Manager
    // Server for incoming requests
    server *tools.GenericGRPCServer
    // configuration
    configuration Config
}

// Set the values of the environment variables.
// TODO Why we need environment variables?
// Deprecated: Use the config elements
func setEnvironmentVars(config *Config) {
    if common.MANAGER_CLUSTER_IP = os.Getenv(utils.MANAGER_ClUSTER_IP); common.MANAGER_CLUSTER_IP == "" {
        log.Fatal().Msgf("%s variable was not set", utils.MANAGER_ClUSTER_IP)
    }

    if common.MANAGER_CLUSTER_PORT = os.Getenv(utils.MANAGER_CLUSTER_PORT); common.MANAGER_CLUSTER_PORT == "" {
        log.Fatal().Msgf("%s variable was not set", utils.MANAGER_CLUSTER_PORT)
        _, err :=  strconv.Atoi(common.MANAGER_CLUSTER_PORT)
        if err != nil {
            log.Fatal().Msgf("%s must be a port number", utils.MANAGER_CLUSTER_PORT)
        }
    }

    if common.CLUSTER_ID = os.Getenv(utils.CLUSTER_ID); common.CLUSTER_ID == "" {
        log.Fatal().Msgf("%s variable was not set", utils.CLUSTER_ID)
    }

    common.CLUSTER_ENV = config.ClusterEnvironment
    common.DEPLOYMENT_MANAGER_ADDR = config.DeploymentMgrAddress
}


func getClusterAPIConnection(hostname string, port int) (*grpc.ClientConn, derrors.Error) {
    // Build connection with cluster API
    tlsConfig := &tls.Config{
        ServerName:   hostname,
        InsecureSkipVerify: true,
    }
    targetAddress := fmt.Sprintf("%s:%d", hostname, port)
    log.Debug().Str("address", targetAddress).Msg("creating cluster API connection")

    creds := credentials.NewTLS(tlsConfig)

    log.Debug().Interface("creds", creds.Info()).Msg("Secure credentials")
    sConn, dErr := grpc.Dial(targetAddress, grpc.WithTransportCredentials(creds))
    if dErr != nil {
        return nil, derrors.AsError(dErr, "cannot create connection with the cluster API service")
    }
    return sConn, nil
}

func NewDeploymentManagerService(config *Config) (*DeploymentManagerService, error) {

    setEnvironmentVars(config)

    vErr := config.Validate()
    if vErr != nil {
        log.Fatal().Str("err", vErr.DebugReport()).Msg("invalid configuration")
    }

    config.Print()

    // login
    clusterAPILoginHelper := login_helper.NewLogin(config.LoginHostname, int(config.LoginPort), config.UseTLSForLogin, config.Email, config.Password)
    err := clusterAPILoginHelper.Login()
    if err != nil {
        log.Panic().Err(err).Msg("there was an error requesting cluster-api login")
        panic(err.Error())
        return nil, err
    }

    exec, kubErr := kubernetes.NewKubernetesExecutor(config.Local)
    if kubErr != nil {
        log.Panic().Err(err).Msg("there was an error creating kubernetes client")
        panic(err.Error())
        return nil, kubErr
    }

    // Build connection with conductor
    log.Debug().Str("hostname", config.ClusterAPIHostname).Msg("connecting with cluster api")
    clusterAPIConn, errCond := getClusterAPIConnection(config.ClusterAPIHostname, int(config.ClusterAPIPort))
    if errCond != nil {
        log.Panic().Err(err).Str("hostname", config.ClusterAPIHostname).Msg("impossible to connect with cluster api")
        panic(err.Error())
        return nil, errCond
    }

    log.Info().Msg("instantiate memory based instances monitor structure...")
    instanceMonitor := monitor.NewMemoryMonitoredInstances()
    log.Info().Msg("done")

    log.Info().Msg("start monitor helper service...")
    monitorService := monitor2.NewMonitorHelper(clusterAPIConn,clusterAPILoginHelper, instanceMonitor)
    go monitorService.Run()
    log.Info().Msg("Done")

    nalejDNSForPods := strings.Split(config.DNS, ",")
    nalejDNSForPods = append(nalejDNSForPods, "8.8.8.8")
    // Instantiate deployment manager service
    mgr := handler.NewManager(&exec, config.ClusterPublicHostname, nalejDNSForPods, instanceMonitor)

    // Instantiate network manager service
    net := network.NewManager(clusterAPIConn, clusterAPILoginHelper)

    // Instantiate target server
    server := tools.NewGenericGRPCServer(config.Port)

    instance := DeploymentManagerService{mgr: mgr, net: net, server: server, configuration: *config}

    return &instance, nil
}


func (d *DeploymentManagerService) Run() {
    // register services

    lis, err := net.Listen("tcp", fmt.Sprintf(":%d", d.configuration.Port))
    if err != nil {
        log.Fatal().Errs("failed to listen: %v", []error{err})
    }

    deployment := handler.NewHandler(d.mgr)
    network := network.NewHandler(d.net)

    // register
    grpcServer := grpc.NewServer()
    pbDeploymentMgr.RegisterDeploymentManagerServer(grpcServer, deployment)
    pbDeploymentMgr.RegisterDeploymentManagerNetworkServer(grpcServer, network)

    reflection.Register(grpcServer)
    // Run
    log.Info().Uint32("port", d.configuration.Port).Msg("Launching gRPC server")
    if err := grpcServer.Serve(lis); err != nil {
        log.Fatal().Errs("failed to serve: %v", []error{err})
    }
}
