package floatingip

import (
	"context"
	"fmt"
	"sort"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/pkg/errors"
	"k8s.io/apiserver/pkg/storage/names"

	infrav1 "github.com/simonswine/cluster-api-provider-hcloud/api/v1alpha3"
	"github.com/simonswine/cluster-api-provider-hcloud/pkg/cloud/scope"
)

type Service struct {
	scope *scope.ClusterScope
}

func NewService(scope *scope.ClusterScope) *Service {
	return &Service{
		scope: scope,
	}
}

var errNotImplemented = errors.New("Not implemented")

type intSlice []int

func matchFloatingIPSpecStatus(spec infrav1.HcloudFloatingIPSpec, status infrav1.HcloudFloatingIPStatus) bool {
	return spec.Type == status.Type
}

func (s intSlice) contains(e int) bool {
	for _, i := range s {
		if i == e {
			return true
		}
	}
	return false
}

func apiToStatus(ip *hcloud.FloatingIP) (*infrav1.HcloudFloatingIPStatus, error) {
	network := fmt.Sprintf("%s/32", ip.IP.String())
	ipString := ip.IP.String()
	if ip.Network != nil {
		network = ip.Network.String()
		networkObj := ip.Network.IP
		networkObj[15] += 1
		ipString = networkObj.String()
	}

	var ipType infrav1.HcloudFloatingIPType

	if ip.Type == hcloud.FloatingIPTypeIPv4 {
		ipType = infrav1.HcloudFloatingIPTypeIPv4
	} else if ip.Type == hcloud.FloatingIPTypeIPv6 {
		ipType = infrav1.HcloudFloatingIPTypeIPv6
	} else {
		return nil, fmt.Errorf("Unknown floating IP type: %s", ip.Type)
	}

	status := &infrav1.HcloudFloatingIPStatus{
		ID:      ip.ID,
		Name:    ip.Name,
		Network: network,
		IP:      ipString,
		Type:    ipType,
		Labels:  ip.Labels,
	}
	return status, nil
}

func (s *Service) Reconcile(ctx context.Context) (err error) {

	// update current status
	floatingIPStatus, err := s.actualStatus(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to refresh floating IPs")
	}
	s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs = floatingIPStatus

	s.scope.V(3).Info("Reconcile floating IPs")
	needCreation, needDeletion := s.compare(floatingIPStatus)

	if len(needCreation) == 0 && len(needDeletion) == 0 {
		return nil
	}

	for _, spec := range needCreation {
		if _, err := s.createFloatingIP(ctx, spec); err != nil {
			return errors.Wrap(err, "failed to create floating IP")
		}
	}

	for _, status := range needDeletion {
		if err := s.deleteFloatingIP(ctx, status); err != nil {
			return errors.Wrap(err, "failed to delete floating IP")
		}
	}

	// update current status
	floatingIPStatus, err = s.actualStatus(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to refresh floating IPs")
	}
	s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs = floatingIPStatus

	return nil
}

func (s *Service) createFloatingIP(ctx context.Context, spec infrav1.HcloudFloatingIPSpec) (*infrav1.HcloudFloatingIPStatus, error) {
	s.scope.V(2).Info("Create a new floating IP", "type", spec.Type)

	// gather ip type
	var ipType hcloud.FloatingIPType
	if spec.Type == infrav1.HcloudFloatingIPTypeIPv4 {
		ipType = hcloud.FloatingIPTypeIPv4
	} else if spec.Type == infrav1.HcloudFloatingIPTypeIPv6 {
		ipType = hcloud.FloatingIPTypeIPv6
	} else {
		return nil, fmt.Errorf("error invalid floating IP type: %s", spec.Type)
	}

	hc := s.scope.HcloudCluster
	clusterTagKey := infrav1.ClusterTagKey(hc.Name)

	if len(hc.Status.Locations) == 0 {
		return nil, errors.New("no locations set on the cluster")
	}
	homeLocation := &hcloud.Location{Name: string(hc.Status.Locations[0])}
	name := names.SimpleNameGenerator.GenerateName(hc.Name + "-control-plane-")
	description := fmt.Sprintf("Kubernetes control plane %s", hc.Name)

	opts := hcloud.FloatingIPCreateOpts{
		Type:         ipType,
		Name:         &name,
		Description:  &description,
		HomeLocation: homeLocation,
		Labels: map[string]string{
			clusterTagKey: string(infrav1.ResourceLifecycleOwned),
		},
	}

	ip, _, err := s.scope.HcloudClient().CreateFloatingIP(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("error creating floating IP: %s", err)
	}

	status, err := apiToStatus(ip.FloatingIP)
	if err != nil {
		return nil, fmt.Errorf("error converting floating IP: %s", err)
	}

	return status, nil
}

func (s *Service) deleteFloatingIP(ctx context.Context, status infrav1.HcloudFloatingIPStatus) error {
	// ensure deleted floating IP is actually owned by us
	clusterTagKey := infrav1.ClusterTagKey(s.scope.HcloudCluster.Name)
	if status.Labels == nil || infrav1.ResourceLifecycle(status.Labels[clusterTagKey]) != infrav1.ResourceLifecycleOwned {
		s.scope.V(3).Info("Ignore request to delete floating IP, as it is not owned", "id", status.ID, "name", status.Name, "network", status.Network)
		return nil
	}
	_, err := s.scope.HcloudClient().DeleteFloatingIP(ctx, &hcloud.FloatingIP{ID: status.ID})
	s.scope.V(2).Info("Delete floating IP", "id", status.ID, "name", status.Name, "network", status.Network)
	return err
}

func (s *Service) Delete(ctx context.Context) (err error) {
	// update current status
	floatingIPStatus, err := s.actualStatus(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to refresh floating IPs")
	}

	for _, status := range floatingIPStatus {
		if err := s.deleteFloatingIP(ctx, status); err != nil {
			return errors.Wrap(err, "failed to delete floating IP")
		}
	}

	return nil
}

func (s *Service) compare(actualStatus []infrav1.HcloudFloatingIPStatus) (needCreation []infrav1.HcloudFloatingIPSpec, needDeletion []infrav1.HcloudFloatingIPStatus) {
	var matchedSpecToStatusMap = make(map[int]*infrav1.HcloudFloatingIPStatus)
	var matchedStatusToSpecMap = make(map[int]*infrav1.HcloudFloatingIPSpec)

	for posSpec, ipSpec := range s.scope.HcloudCluster.Spec.ControlPlaneFloatingIPs {
		for posStatus, ipStatus := range s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs {
			if _, ok := matchedStatusToSpecMap[posStatus]; ok {
				continue
			}

			if matchFloatingIPSpecStatus(ipSpec, ipStatus) {
				matchedStatusToSpecMap[posStatus] = &ipSpec
				matchedSpecToStatusMap[posSpec] = &ipStatus
			}
		}
	}

	// floating IPs to create
	for pos, spec := range s.scope.HcloudCluster.Spec.ControlPlaneFloatingIPs {
		if _, ok := matchedSpecToStatusMap[pos]; ok {
			continue
		}
		needCreation = append(needCreation, spec)
	}

	for pos, status := range s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs {
		if _, ok := matchedStatusToSpecMap[pos]; ok {
			continue
		}
		needDeletion = append(needDeletion, status)
	}

	return needCreation, needDeletion
}

// actualStatus gathers all floating IPs referenced by ID on object or a
// appropriate tag and converts them into the status object
func (s *Service) actualStatus(ctx context.Context) ([]infrav1.HcloudFloatingIPStatus, error) {
	// index existing status entries
	var ids intSlice
	ipStatusByID := make(map[int]*infrav1.HcloudFloatingIPStatus)
	for pos := range s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs {
		ipStatus := &s.scope.HcloudCluster.Status.ControlPlaneFloatingIPs[pos]
		ipStatusByID[ipStatus.ID] = ipStatus
		ids = append(ids, ipStatus.ID)
	}
	for _, ipSpec := range s.scope.HcloudCluster.Spec.ControlPlaneFloatingIPs {
		if ipSpec.ID != nil {
			ids = append(ids, *ipSpec.ID)
		}
	}

	// refresh existing floating IPs
	clusterTagKey := infrav1.ClusterTagKey(s.scope.HcloudCluster.Name)
	ipStatuses, err := s.scope.HcloudClient().ListFloatingIPs(ctx, hcloud.FloatingIPListOpts{})
	if err != nil {
		return nil, fmt.Errorf("error listing floating IPs: %w", err)
	}
	for _, ipStatus := range ipStatuses {
		_, ok := ipStatus.Labels[clusterTagKey]
		if !ok && !ids.contains(ipStatus.ID) {
			continue
		}

		apiStatus, err := apiToStatus(ipStatus)
		if err != nil {
			return nil, fmt.Errorf("error converting floatingIP API to status: %w", err)
		}
		ipStatusByID[apiStatus.ID] = apiStatus
	}

	ids = []int{}
	for id := range ipStatusByID {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	var floatingIPs []infrav1.HcloudFloatingIPStatus

	for _, id := range ids {
		status := ipStatusByID[id]
		floatingIPs = append(
			floatingIPs,
			*status,
		)
	}

	return floatingIPs, nil
}
