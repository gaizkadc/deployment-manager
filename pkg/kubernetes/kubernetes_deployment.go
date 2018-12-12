/*
 *  Copyright (C) 2018 Nalej Group - All Rights Reserved
 *
 *
 */

package kubernetes

import (
    pbConductor "github.com/nalej/grpc-conductor-go"
    appsv1 "k8s.io/api/apps/v1"
    apiv1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "github.com/nalej/deployment-manager/pkg/executor"
    "github.com/rs/zerolog/log"
    "k8s.io/client-go/kubernetes/typed/apps/v1"
    "k8s.io/client-go/kubernetes"
    "github.com/nalej/deployment-manager/pkg"
    "github.com/nalej/deployment-manager/pkg/utils"
    "fmt"
    "github.com/nalej/deployment-manager/pkg/network"
)

const (
    // Name of the Docker ZT agent image
    ZTAgentImageName = "nalejops/zt-agent:v0.1.0"
)


// Deployable deployments
//-----------------------

type DeployableDeployments struct{
    // kubernetes Client
    client v1.DeploymentInterface
    // stage associated with these resources
    stage *pbConductor.DeploymentStage
    // namespace name descriptor
    targetNamespace string
    // zero-tier network id
    ztNetworkId string
    // organization id
    organizationId string
    // organization name
    organizationName string
    // deployment id
    deploymentId string
    // application instance id
    appInstanceId string
    // app nalej name
    appName string
    // map of deployments ready to be deployed
    // service_id -> deployment
    deployments map[string]appsv1.Deployment
    // map of agents deployed for every service
    // service_id -> zt-agent deployment
    ztAgents map[string]appsv1.Deployment
}

func NewDeployableDeployment(client *kubernetes.Clientset, stage *pbConductor.DeploymentStage,
    targetNamespace string, ztNetworkId string, organizationId string, organizationName string,
    deploymentId string, appInstanceId string, appName string) *DeployableDeployments {
    return &DeployableDeployments{
        client: client.AppsV1().Deployments(targetNamespace),
        stage: stage,
        targetNamespace: targetNamespace,
        ztNetworkId: ztNetworkId,
        organizationId: organizationId,
        organizationName: organizationName,
        deploymentId: deploymentId,
        appInstanceId: appInstanceId,
        appName: appName,
        deployments: make(map[string]appsv1.Deployment,0),
        ztAgents: make(map[string]appsv1.Deployment,0),
    }
}

func(d *DeployableDeployments) GetId() string {
    return d.stage.StageId
}

func(d *DeployableDeployments) Build() error {

    deployments:= make(map[string]appsv1.Deployment,0)
    agents:= make(map[string]appsv1.Deployment,0)

    // Create the list of Nalej variables
    nalejVars := d.getNalejEnvVariables()

    for serviceIndex, service := range d.stage.Services {
        log.Debug().Msgf("build deployment %s %d out of %d",service.ServiceId,serviceIndex+1,len(d.stage.Services))

        // value for privileged user
        user0 := int64(0)
        privilegedUser := &user0

        deployment := appsv1.Deployment{
            ObjectMeta: metav1.ObjectMeta{
                Name: pkg.FormatName(service.Name),
                Namespace: d.targetNamespace,
                Labels: service.Labels,
                Annotations: map[string] string {
                    utils.NALEJ_SERVICE_NAME : service.ServiceId,
                },
            },
            Spec: appsv1.DeploymentSpec{
                Replicas: int32Ptr(service.Specs.Replicas),
                Selector: &metav1.LabelSelector{
                    MatchLabels: service.Labels,
                },
                Template: apiv1.PodTemplateSpec{
                    ObjectMeta: metav1.ObjectMeta{
                        Labels: service.Labels,
                    },
                    Spec: apiv1.PodSpec{
                        Containers: []apiv1.Container{
                            // User defined container
                            {
                                Name:  pkg.FormatName(service.Name),
                                Image: service.Image,
                                Env:   d.getEnvVariables(nalejVars,service.EnvironmentVariables),
                                Ports: d.getContainerPorts(service.ExposedPorts),
                            },
                            // ZT sidecar container
                            {
                                Name: "zt-sidecar",
                                Image: ZTAgentImageName,
                                Args: []string{
                                    "run",
                                    "--appInstanceId", d.appInstanceId,
                                    "--appName", d.appName,
                                    "--serviceName", service.Name,
                                    "--deploymentId", d.deploymentId,
                                    "--fragmentId", d.stage.FragmentId,
                                    "--managerAddr", pkg.DEPLOYMENT_MANAGER_ADDR,
                                    "--organizationId", d.organizationId,
                                    "--organizationName", d.organizationName,
                                    "--networkId", d.ztNetworkId,
                                },
                                Env: []apiv1.EnvVar{
                                    // Indicate this is not a ZT proxy
                                    {
                                        Name:  "ZT_PROXY",
                                        Value: "false",
                                    },
                                },
                                // The proxy exposes the same ports of the deployment
                                Ports: d.getContainerPorts(service.ExposedPorts),
                                SecurityContext:
                                &apiv1.SecurityContext{
                                    RunAsUser: privilegedUser,
                                    Privileged: boolPtr(true),
                                    Capabilities: &apiv1.Capabilities{
                                        Add: [] apiv1.Capability{
                                            "NET_ADMIN",
                                            "SYS_ADMIN",
                                        },
                                    },
                                },

                                VolumeMounts: []apiv1.VolumeMount{
                                    {
                                        Name: "dev-net-tun",
                                        ReadOnly: true,
                                        MountPath: "/dev/net/tun",
                                    },
                                },
                            },
                        },
                        Volumes: []apiv1.Volume{
                            // zerotier sidecar volume
                            {
                                Name: "dev-net-tun",
                                VolumeSource: apiv1.VolumeSource{
                                    HostPath: &apiv1.HostPathVolumeSource{
                                        Path: "/dev/net/tun",
                                    },
                                },
                            },
                        },
                    },
                },
            },
        }
        // Set a different set of labels to identify this agent
        ztAgentLabels := map[string]string {
            "agent": "zt-agent",
            "app": service.Labels["app"],
        }

        ztAgentName := fmt.Sprintf("zt-%s",pkg.FormatName(service.Name))
        agent := appsv1.Deployment{
            ObjectMeta: metav1.ObjectMeta{
                Name: ztAgentName,
                Namespace: d.targetNamespace,
                Labels: ztAgentLabels,
                Annotations: map[string] string {
                    utils.NALEJ_SERVICE_NAME : service.ServiceId,
                },
            },
            Spec: appsv1.DeploymentSpec{
                Replicas: int32Ptr(1),
                Selector: &metav1.LabelSelector{
                    MatchLabels:ztAgentLabels,
                },
                Template: apiv1.PodTemplateSpec{
                    ObjectMeta: metav1.ObjectMeta{
                        Labels: ztAgentLabels,
                    },
                    // Every pod template is designed to use a container with the requested image
                    // and a helping sidecar with a containerized zerotier that joins the network
                    // after running
                    Spec: apiv1.PodSpec{
                        Containers: []apiv1.Container{
                            // zero-tier sidecar
                            {
                                Name: ztAgentName,
                                Image: ZTAgentImageName,
                                Args: []string{
                                    "run",
                                    "--appInstanceId", d.appInstanceId,
                                    "--appName", d.appName,
                                    "--serviceName", pkg.FormatName(service.Name),
                                    "--deploymentId", d.deploymentId,
                                    "--fragmentId", d.stage.FragmentId,
                                    "--managerAddr", pkg.DEPLOYMENT_MANAGER_ADDR,
                                    "--organizationId", d.organizationId,
                                    "--organizationName", d.organizationName,
                                    "--networkId", d.ztNetworkId,
                                    "--isProxy",
                                },
                                Env: []apiv1.EnvVar{
                                    // Indicate this is a ZT proxy
                                    {
                                        Name:  "ZT_PROXY",
                                        Value: "true",
                                    },
                                    // Indicate the name of the k8s service
                                    {
                                        Name: "K8S_SERVICE_NAME",
                                        Value: pkg.FormatName(service.Name),
                                    },
                                },
                                // The proxy exposes the same ports of the deployment
                                Ports: d.getContainerPorts(service.ExposedPorts),
                                SecurityContext:
                                &apiv1.SecurityContext{
                                    RunAsUser: privilegedUser,
                                    Privileged: boolPtr(true),
                                    Capabilities: &apiv1.Capabilities{
                                        Add: [] apiv1.Capability{
                                            "NET_ADMIN",
                                            "SYS_ADMIN",
                                        },
                                    },
                                },

                                VolumeMounts: []apiv1.VolumeMount{
                                    {
                                        Name: "dev-net-tun",
                                        ReadOnly: true,
                                        MountPath: "/dev/net/tun",
                                    },
                                },
                            },
                        },

                        Volumes: []apiv1.Volume{
                            // zerotier sidecar volume
                            {
                                Name: "dev-net-tun",
                                VolumeSource: apiv1.VolumeSource{
                                    HostPath: &apiv1.HostPathVolumeSource{
                                        Path: "/dev/net/tun",
                                    },
                                },
                            },
                        },
                    },
                },
            },
        }

        deployments[service.ServiceId] = deployment
        agents[service.ServiceId] = agent
    }


    d.deployments = deployments
    d.ztAgents = agents
    return nil
}

func(d *DeployableDeployments) Deploy(controller executor.DeploymentController) error {
    for serviceId, dep := range d.deployments {
        deployed, err := d.client.Create(&dep)
        if err != nil {
            log.Error().Err(err).Msgf("error creating deployment %s",dep.Name)
            return err
        }
        log.Debug().Msgf("created deployment with uid %s", deployed.GetUID())
        controller.AddMonitoredResource(string(deployed.GetUID()), serviceId, d.stage.StageId)
    }
    // same approach for agents
    for serviceId, dep := range d.ztAgents {
        deployed, err := d.client.Create(&dep)
        if err != nil {
            log.Error().Err(err).Msgf("error creating deployment for zt-agent %s",dep.Name)
            return err
        }
        log.Debug().Msgf("created deployment with uid %s", deployed.GetUID())
        controller.AddMonitoredResource(string(deployed.GetUID()), serviceId, d.stage.StageId)
    }
    return nil
}

func(d *DeployableDeployments) Undeploy() error {
    for _, dep := range d.deployments {
        err := d.client.Delete(dep.Name,metav1.NewDeleteOptions(DeleteGracePeriod))
        if err != nil {
            log.Error().Err(err).Msgf("error creating deployment %s",dep.Name)
            return err
        }
    }
    return nil
}

// Get the set of Nalej environment variables designed to help users.
//  params:
//   nalejVariables set of variables for nalej services
//  return:
//   list of environment variables
func (d *DeployableDeployments) getNalejEnvVariables() [] apiv1.EnvVar {
    vars := make([]apiv1.EnvVar,0)
    for service_id, service := range d.stage.Services {
        aux := apiv1.EnvVar{
            // Create a variable like NALEJ_SERV_MYSQL: mysql-org1
            //Name: fmt.Sprintf("NALEJ_SERV_%s", strings.ToUpper(pkg.FormatName(service.Name))),
            Name: fmt.Sprintf("NALEJ_SERV_%d", service_id),
            Value: network.GetNetworkingName(service.Name, d.organizationName, d.appInstanceId),
        }
        vars = append(vars, aux)
    }
    log.Debug().Interface("variables", vars).Msg("Nalej common variables")
    return vars
}


// Transform a service map of environment variables to the corresponding K8s API structure.
//  params:
//   variables to be used
//  return:
//   list of k8s environment variables
func (d *DeployableDeployments) getEnvVariables(nalejVariables []apiv1.EnvVar, variables map[string]string) []apiv1.EnvVar {
    result := make([]apiv1.EnvVar, 0)
    for k, v := range variables {
        log.Debug().Str(k,v).Msg("user defined variable")
        result = append(result, apiv1.EnvVar{Name: k, Value: v})
    }
    for _, e := range nalejVariables {
        result = append(result, e)
    }
    log.Debug().Interface("nalej_variables", result).Str("appId", d.appInstanceId).Msg("generated variables for service")
    return result
}


// Transform a Nalej list of exposed ports into a K8s api port.
//  params:
//   ports list of exposed ports
//  return:
//   list of ports into k8s api format
func (d *DeployableDeployments) getContainerPorts(ports []*pbConductor.Port) []apiv1.ContainerPort {
    obtained := make([]apiv1.ContainerPort, 0, len(ports))
    for _, p := range ports {
        obtained = append(obtained, apiv1.ContainerPort{ContainerPort: p.ExposedPort, Name: p.Name})
    }
    return obtained
}