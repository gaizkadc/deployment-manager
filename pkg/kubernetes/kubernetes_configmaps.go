/*
 *  Copyright (C) 2018 Nalej Group - All Rights Reserved
 */

package kubernetes

import (
	"github.com/nalej/deployment-manager/internal/entities"
	"github.com/nalej/deployment-manager/pkg/executor"
	"github.com/nalej/deployment-manager/pkg/utils"
	"github.com/nalej/grpc-application-go"
	"github.com/rs/zerolog/log"
	"k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coreV1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"strings"
)

type DeployableConfigMaps struct {
	client          coreV1.ConfigMapInterface
	data            entities.DeploymentMetadata
	configmaps      map[string][]*v1.ConfigMap
}

func NewDeployableConfigMaps(
	client *kubernetes.Clientset,
	data entities.DeploymentMetadata) *DeployableConfigMaps {
	return &DeployableConfigMaps{
		client:          client.CoreV1().ConfigMaps(data.Namespace),
		data:            data,
		configmaps:      make(map[string][]*v1.ConfigMap, 0),
	}
}

func (dc *DeployableConfigMaps) GetId() string {
	return dc.data.Stage.StageId
}

func GetConfigMapPath(mountPath string) (string, string) {
	index := strings.LastIndex(mountPath, "/")
	if index == -1 {
		return "/", mountPath
	}
	return mountPath [0: index + 1], mountPath [index + 1: ]
}

func (dc *DeployableConfigMaps) generateConfigMap(serviceId string, serviceInstanceId string, cf *grpc_application_go.ConfigFile) *v1.ConfigMap {
	log.Debug().Interface("configMap", cf).Msg("generating config map...")
	_, file := GetConfigMapPath(cf.MountPath)
	log.Debug().Str("file", file).Msg("Config map content")
	return &v1.ConfigMap{
		TypeMeta: v12.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: v12.ObjectMeta{
			Name:      cf.ConfigFileId,
			Namespace: dc.data.Namespace,
			Labels:    map[string]string{
				utils.NALEJ_ANNOTATION_ORGANIZATION : dc.data.OrganizationId,
				utils.NALEJ_ANNOTATION_APP_DESCRIPTOR : dc.data.AppDescriptorId,
				utils.NALEJ_ANNOTATION_APP_INSTANCE_ID : dc.data.AppInstanceId,
				utils.NALEJ_ANNOTATION_STAGE_ID : dc.data.Stage.StageId,
				utils.NALEJ_ANNOTATION_SERVICE_ID : serviceId,
				utils.NALEJ_ANNOTATION_SERVICE_INSTANCE_ID : serviceInstanceId,
				utils.NALEJ_ANNOTATION_SERVICE_GROUP_ID : dc.data.ServiceGroupId,
				utils.NALEJ_ANNOTATION_SERVICE_GROUP_INSTANCE_ID : dc.data.ServiceGroupInstanceId,
			},
		},
		BinaryData: map[string][]byte{
			file: cf.Content,
		},
	}
}

func (dc *DeployableConfigMaps) BuildConfigMapsForService(service *grpc_application_go.ServiceInstance) []*v1.ConfigMap {
	if len(service.Configs) == 0 {
		return nil
	}
	cms := make([]*v1.ConfigMap, 0)
	for _, cm := range service.Configs {
		toAdd := dc.generateConfigMap(service.ServiceId, service.ServiceInstanceId, cm)
		if toAdd != nil {
			log.Debug().Interface("toAdd", toAdd).Str("serviceName", service.Name).Msg("Adding new config file")
			cms = append(cms, toAdd)
		}
	}
	log.Debug().Interface("number", len(cms)).Str("serviceName", service.Name).Msg("Config maps prepared for service")
	return cms
}

func (dc *DeployableConfigMaps) Build() error {
	for _, service := range dc.data.Stage.Services {
		toAdd := dc.BuildConfigMapsForService(service)
		if toAdd != nil && len(toAdd) > 0 {
			dc.configmaps[service.ServiceId] = toAdd
		}
	}
	log.Debug().Interface("Configmaps", dc.configmaps).Msg("configmap have been build and are ready to deploy")
	return nil
}

func (dc *DeployableConfigMaps) Deploy(controller executor.DeploymentController) error {
	numCreated := 0
	for serviceId, configmaps := range dc.configmaps {
		for _, toCreate := range configmaps {
			log.Debug().Interface("toCreate", toCreate).Msg("creating config map")
			created, err := dc.client.Create(toCreate)
			if err != nil {
				log.Error().Err(err).Interface("toCreate", toCreate).Msg("cannot create config map")
				return err
			}
			log.Debug().Str("serviceId", serviceId).Str("uid", string(created.GetUID())).Msg("Configmap has been created")
			numCreated++
		}
	}
	log.Debug().Int("created", numCreated).Msg("Configmaps have been created")
	return nil
}

func (dc *DeployableConfigMaps) Undeploy() error {
	deleted := 0
	for serviceId, configmaps := range dc.configmaps {
		for _, toDelete := range configmaps {
			err := dc.client.Delete(toDelete.Name, metaV1.NewDeleteOptions(DeleteGracePeriod))
			if err != nil {
				log.Error().Str("serviceId", serviceId).Interface("toDelete", toDelete).Msg("cannot delete configmap")
				return err

			}
			log.Debug().Str("serviceId", serviceId).Str("Name", toDelete.Name).Msg("Configmap has been deleted")
		}
		deleted++
	}
	log.Debug().Int("deleted", deleted).Msg("Configmaps have been deleted")
	return nil
}
