package controller

import (
	"errors"
	"fmt"

	"github.com/containous/maesh/pkg/k8s"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
)

// ShadowServiceManager manages shadow services.
type ShadowServiceManager struct {
	lister        listers.ServiceLister
	namespace     string
	tcpStateTable TCPPortMapper
	defaultMode   string
	minHTTPPort   int32
	maxHTTPPort   int32
	kubeClient    kubernetes.Interface
}

// NewShadowServiceManager returns new shadow service manager.
func NewShadowServiceManager(lister listers.ServiceLister, namespace string, tcpStateTable TCPPortMapper, defaultMode string, minHTTPPort, maxHTTPPort int32, kubeClient kubernetes.Interface) *ShadowServiceManager {
	return &ShadowServiceManager{
		lister:        lister,
		namespace:     namespace,
		tcpStateTable: tcpStateTable,
		defaultMode:   defaultMode,
		minHTTPPort:   minHTTPPort,
		maxHTTPPort:   maxHTTPPort,
		kubeClient:    kubeClient,
	}
}

// Create creates a new shadow service based on the given service.
func (s *ShadowServiceManager) Create(userSvc *corev1.Service) error {
	name := s.getShadowServiceName(userSvc.Name, userSvc.Namespace)

	log.Debugf("Creating mesh service: %s", name)

	_, err := s.lister.Services(s.namespace).Get(name)
	if err == nil {
		return nil
	}

	if !kerrors.IsNotFound(err) {
		return fmt.Errorf("unable to get shadow service %q: %w", name, err)
	}

	ports := s.getShadowServicePorts(userSvc)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app": "maesh",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: ports,
			Selector: map[string]string{
				"component": "maesh-mesh",
			},
		},
	}

	if _, err = s.kubeClient.CoreV1().Services(s.namespace).Create(svc); err != nil {
		return fmt.Errorf("unable to create kubernetes service: %w", err)
	}

	return nil
}

// Update updates the shadow service associated with the old user service following the content of the new user service.
func (s *ShadowServiceManager) Update(oldUserSvc *v1.Service, newUserSvc *v1.Service) (*v1.Service, error) {
	name := s.getShadowServiceName(newUserSvc.Name, newUserSvc.Namespace)

	s.cleanupPortMapping(oldUserSvc, newUserSvc)

	var updatedSvc *corev1.Service

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := s.lister.Services(s.namespace).Get(name)
		if err != nil {
			return fmt.Errorf("unable to get shadow service %q: %w", name, err)
		}

		newSvc := svc.DeepCopy()
		newSvc.Spec.Ports = s.getShadowServicePorts(newUserSvc)

		if updatedSvc, err = s.kubeClient.CoreV1().Services(s.namespace).Update(newSvc); err != nil {
			return fmt.Errorf("unable to update kubernetes service: %w", err)
		}

		return nil
	})

	if retryErr != nil {
		return nil, fmt.Errorf("unable to update service %q: %v", name, retryErr)
	}

	log.Debugf("Updated service: %s/%s", s.namespace, name)

	return updatedSvc, nil
}

// Delete deletes a shadow service associated with the given user service.
func (s *ShadowServiceManager) Delete(userSvc *v1.Service) error {
	name := s.getShadowServiceName(userSvc.Name, userSvc.Namespace)

	s.cleanupPortMapping(userSvc, nil)

	_, err := s.lister.Services(s.namespace).Get(name)
	if err != nil {
		return err
	}

	if err := s.kubeClient.CoreV1().Services(s.namespace).Delete(name, &metav1.DeleteOptions{}); err != nil {
		return err
	}

	log.Debugf("Deleted service: %s/%s", s.namespace, name)

	return nil
}

func (s *ShadowServiceManager) cleanupPortMapping(oldUserSvc *corev1.Service, newUserSvc *corev1.Service) {
	for _, old := range oldUserSvc.Spec.Ports {
		var found bool

		if newUserSvc != nil {
			for _, new := range newUserSvc.Spec.Ports {
				if old.Port == new.Port {
					found = true
					break
				}
			}
		}

		if !found {
			_, err := s.tcpStateTable.Remove(k8s.ServiceWithPort{
				Namespace: oldUserSvc.Namespace,
				Name:      oldUserSvc.Name,
				Port:      old.Port,
			})
			if err != nil {
				log.Warnf("Unable to remove port mapping for %s/%s on port %d", oldUserSvc.Namespace, oldUserSvc.Name, old.Port)
			}
		}
	}
}

func (s *ShadowServiceManager) getShadowServicePorts(svc *corev1.Service) []corev1.ServicePort {
	var ports []corev1.ServicePort

	svcMode := svc.Annotations[k8s.AnnotationServiceType]
	if svcMode == "" {
		svcMode = s.defaultMode
	}

	for i, sp := range svc.Spec.Ports {
		if sp.Protocol != corev1.ProtocolTCP {
			log.Warnf("Unsupported port type: %s, skipping port %s on service %s/%s", sp.Protocol, sp.Name, svc.Namespace, svc.Name)
			continue
		}

		targetPort, err := s.getTargetPort(svcMode, i, svc.Name, svc.Namespace, sp.Port)
		if err != nil {
			log.Errorf("Unable to find available %s port: %v, skipping port %s on service %s/%s", sp.Name, err, sp.Name, svc.Namespace, svc.Name)
			continue
		}

		ports = append(ports, corev1.ServicePort{
			Name:       sp.Name,
			Port:       sp.Port,
			Protocol:   sp.Protocol,
			TargetPort: intstr.FromInt(int(targetPort)),
		})
	}

	return ports
}

// userServiceToMeshServiceName converts a User service with a namespace to a mesh service name.
func (s *ShadowServiceManager) getShadowServiceName(name string, namespace string) string {
	return fmt.Sprintf("%s-%s-6d61657368-%s", s.namespace, name, namespace)
}

func (s *ShadowServiceManager) getTargetPort(svcMode string, portID int, name, namespace string, port int32) (int32, error) {
	switch svcMode {
	case k8s.ServiceTypeHTTP:
		return s.getHTTPPort(portID)
	case k8s.ServiceTypeTCP:
		return s.getTCPPort(name, namespace, port)
	default:
		return 0, errors.New("unknown service mode")
	}
}

// GetHTTPPort returns the HTTP port associated with the given portId.
func (s *ShadowServiceManager) getHTTPPort(portID int) (int32, error) {
	if s.minHTTPPort+int32(portID) >= s.maxHTTPPort {
		return 0, errors.New("unable to find an available HTTP port")
	}

	return s.minHTTPPort + int32(portID), nil
}

// GetTCPPort returns the TCP port associated with the given service information.
func (s *ShadowServiceManager) getTCPPort(svcName, svcNamespace string, svcPort int32) (int32, error) {
	svc := k8s.ServiceWithPort{
		Namespace: svcNamespace,
		Name:      svcName,
		Port:      svcPort,
	}
	if port, ok := s.tcpStateTable.Find(svc); ok {
		return port, nil
	}

	log.Debugf("No match found for %s/%s %d - Add a new port", svcName, svcNamespace, svcPort)

	port, err := s.tcpStateTable.Add(&svc)
	if err != nil {
		return 0, fmt.Errorf("unable to add service to the TCP state table: %w", err)
	}

	log.Debugf("Service %s/%s %d as been assigned port %d", svcName, svcNamespace, svcPort, port)

	return port, nil
}
