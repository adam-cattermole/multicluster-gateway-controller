package dnspolicy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/kuadrant/kuadrant-operator/pkg/common"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/_internal/slice"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/dns"
)

const (
	LabelGatewayReference  = "kuadrant.io/gateway"
	LabelGatewayNSRef      = "kuadrant.io/gateway-namespace"
	LabelListenerReference = "kuadrant.io/listener-name"
)

var (
	ErrUnknownRoutingStrategy = fmt.Errorf("unknown routing strategy")
	ErrNoManagedZoneForHost   = fmt.Errorf("no managed zone for host")
	ErrAlreadyAssigned        = fmt.Errorf("managed host already assigned")
)

type dnsHelper struct {
	client.Client
}

func findMatchingManagedZone(originalHost, host string, zones []v1alpha1.ManagedZone) (*v1alpha1.ManagedZone, string, error) {
	if len(zones) == 0 {
		return nil, "", fmt.Errorf("%w : %s", ErrNoManagedZoneForHost, host)
	}
	host = strings.ToLower(host)
	//get the TLD from this host
	tld, _ := publicsuffix.PublicSuffix(host)

	//The host is a TLD, so we now know `originalHost` can't possibly have a valid `ManagedZone` available.
	if host == tld {
		return nil, "", fmt.Errorf("no valid zone found for host: %v", originalHost)
	}

	hostParts := strings.SplitN(host, ".", 2)
	if len(hostParts) < 2 {
		return nil, "", fmt.Errorf("no valid zone found for host: %s", originalHost)
	}
	parentDomain := hostParts[1]

	// We do not currently support creating records for Apex domains, and a ManagedZone represents an Apex domain, as such
	// we should never be trying to find a managed zone that matches the `originalHost` exactly. Instead, we just continue
	// on to the next possible valid host to try i.e. the parent domain.
	if host == originalHost {
		return findMatchingManagedZone(originalHost, parentDomain, zones)
	}

	zone, ok := slice.Find(zones, func(zone v1alpha1.ManagedZone) bool {
		return strings.ToLower(zone.Spec.DomainName) == host
	})

	if ok {
		subdomain := strings.Replace(strings.ToLower(originalHost), "."+strings.ToLower(zone.Spec.DomainName), "", 1)
		return &zone, subdomain, nil
	}
	return findMatchingManagedZone(originalHost, parentDomain, zones)

}

func commonDNSRecordLabels(gwKey, apKey client.ObjectKey) map[string]string {
	common := map[string]string{}
	for k, v := range policyDNSRecordLabels(apKey) {
		common[k] = v
	}
	for k, v := range gatewayDNSRecordLabels(gwKey) {
		common[k] = v
	}
	return common
}

func policyDNSRecordLabels(apKey client.ObjectKey) map[string]string {
	return map[string]string{
		DNSPolicyBackRefAnnotation:                              apKey.Name,
		fmt.Sprintf("%s-namespace", DNSPolicyBackRefAnnotation): apKey.Namespace,
	}
}

func gatewayDNSRecordLabels(gwKey client.ObjectKey) map[string]string {
	return map[string]string{
		LabelGatewayNSRef:     gwKey.Namespace,
		LabelGatewayReference: gwKey.Name,
	}
}

func (dh *dnsHelper) buildDNSRecordForListener(gateway *gatewayapiv1.Gateway, dnsPolicy *v1alpha1.DNSPolicy, targetListener gatewayapiv1.Listener, managedZone *v1alpha1.ManagedZone) *v1alpha1.DNSRecord {

	dnsRecord := &v1alpha1.DNSRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsRecordName(gateway.Name, string(targetListener.Name)),
			Namespace: managedZone.Namespace,
			Labels:    commonDNSRecordLabels(client.ObjectKeyFromObject(gateway), client.ObjectKeyFromObject(dnsPolicy)),
		},
		Spec: v1alpha1.DNSRecordSpec{
			ManagedZoneRef: &v1alpha1.ManagedZoneReference{
				Name: managedZone.Name,
			},
		},
	}
	dnsRecord.Labels[LabelListenerReference] = string(targetListener.Name)
	return dnsRecord
}

// getDNSRecordForListener returns a v1alpha1.DNSRecord, if one exists, for the given listener in the given v1alpha1.ManagedZone.
func (dh *dnsHelper) getDNSRecordForListener(ctx context.Context, listener gatewayapiv1.Listener, owner metav1.Object) (*v1alpha1.DNSRecord, error) {
	recordName := dnsRecordName(owner.GetName(), string(listener.Name))
	dnsRecord := &v1alpha1.DNSRecord{}
	if err := dh.Get(ctx, client.ObjectKey{Name: recordName, Namespace: owner.GetNamespace()}, dnsRecord); err != nil {
		if k8serrors.IsNotFound(err) {
			log.Log.V(1).Info("no dnsrecord found for listener ", "listener", listener)
		}
		return nil, err
	}
	return dnsRecord, nil
}

func withGatewayListener[T metav1.Object](gateway common.GatewayWrapper, listener gatewayapiv1.Listener, obj T) T {
	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}

	obj.GetAnnotations()["dnsrecord-name"] = fmt.Sprintf("%s-%s", gateway.Name, listener.Name)
	obj.GetAnnotations()["dnsrecord-namespace"] = gateway.Namespace

	return obj
}

func (dh *dnsHelper) setEndpoints(ctx context.Context, mcgTarget *dns.MultiClusterGatewayTarget, dnsRecord *v1alpha1.DNSRecord, listener gatewayapiv1.Listener, strategy v1alpha1.RoutingStrategy) error {
	old := dnsRecord.DeepCopy()
	gwListenerHost := string(*listener.Hostname)
	var endpoints []*v1alpha1.Endpoint

	//Health Checks currently modify endpoints so we have to keep existing ones in order to not lose health check ids
	currentEndpoints := make(map[string]*v1alpha1.Endpoint, len(dnsRecord.Spec.Endpoints))
	for _, endpoint := range dnsRecord.Spec.Endpoints {
		currentEndpoints[endpoint.SetID()] = endpoint
	}

	switch strategy {
	case v1alpha1.SimpleRoutingStrategy:
		endpoints = dh.getSimpleEndpoints(mcgTarget, gwListenerHost, currentEndpoints)
	case v1alpha1.LoadBalancedRoutingStrategy:
		endpoints = dh.getLoadBalancedEndpoints(mcgTarget, gwListenerHost, currentEndpoints)
	default:
		return fmt.Errorf("%w : %s", ErrUnknownRoutingStrategy, strategy)
	}

	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].SetID() < endpoints[j].SetID()
	})

	dnsRecord.Spec.Endpoints = endpoints

	if !equality.Semantic.DeepEqual(old, dnsRecord) {
		return dh.Update(ctx, dnsRecord)
	}

	return nil
}

// getSimpleEndpoints returns the endpoints for the given MultiClusterGatewayTarget using the simple routing strategy

func (dh *dnsHelper) getSimpleEndpoints(mcgTarget *dns.MultiClusterGatewayTarget, hostname string, currentEndpoints map[string]*v1alpha1.Endpoint) []*v1alpha1.Endpoint {

	var (
		endpoints  []*v1alpha1.Endpoint
		ipValues   []string
		hostValues []string
	)

	for _, cgwTarget := range mcgTarget.ClusterGatewayTargets {
		for _, gwa := range cgwTarget.Status.Addresses {
			if *gwa.Type == gatewayapiv1.IPAddressType {
				ipValues = append(ipValues, gwa.Value)
			} else {
				hostValues = append(hostValues, gwa.Value)
			}
		}
	}

	if len(ipValues) > 0 {
		endpoint := createOrUpdateEndpoint(hostname, ipValues, v1alpha1.ARecordType, "", dns.DefaultTTL, currentEndpoints)
		endpoints = append(endpoints, endpoint)
	}

	//ToDO This could possibly result in an invalid record since you can't have multiple CNAME target values https://github.com/Kuadrant/multicluster-gateway-controller/issues/663
	if len(hostValues) > 0 {
		endpoint := createOrUpdateEndpoint(hostname, hostValues, v1alpha1.CNAMERecordType, "", dns.DefaultTTL, currentEndpoints)
		endpoints = append(endpoints, endpoint)
	}

	return endpoints
}

// getLoadBalancedEndpoints returns the endpoints for the given MultiClusterGatewayTarget using the loadbalanced routing strategy
//
// Builds an array of v1alpha1.Endpoint resources and sets them on the given DNSRecord. The endpoints expected are calculated
// from the MultiClusterGatewayTarget using the target Gateway (MultiClusterGatewayTarget.Gateway), the LoadBalancing Spec
// from the DNSPolicy attached to the target gateway (MultiClusterGatewayTarget.LoadBalancing) and the list of clusters the
// target gateway is currently placed on (MultiClusterGatewayTarget.ClusterGatewayTargets).
//
// MultiClusterGatewayTarget.ClusterGatewayTarget are grouped by Geo, in the case of Geo not being defined in the
// LoadBalancing Spec (Weighted only) an internal only Geo Code of "default" is used and all clusters added to it.
//
// A CNAME record is created for the target host (DNSRecord.name), pointing to a generated gateway lb host.
// A CNAME record for the gateway lb host is created for every Geo, with appropriate Geo information, pointing to a geo
// specific host.
// A CNAME record for the geo specific host is created for every Geo, with weight information for that target added,
// pointing to a target cluster hostname.
// An A record for the target cluster hostname is created for any IP targets retrieved for that cluster.
//
// Example(Weighted only)
//
// www.example.com CNAME lb-1ab1.www.example.com
// lb-1ab1.www.example.com CNAME geolocation * default.lb-1ab1.www.example.com
// default.lb-1ab1.www.example.com CNAME weighted 100 1bc1.lb-1ab1.www.example.com
// default.lb-1ab1.www.example.com CNAME weighted 100 aws.lb.com
// 1bc1.lb-1ab1.www.example.com A 192.22.2.1
//
// Example(Geo, default IE)
//
// shop.example.com CNAME lb-a1b2.shop.example.com
// lb-a1b2.shop.example.com CNAME geolocation ireland ie.lb-a1b2.shop.example.com
// lb-a1b2.shop.example.com geolocation australia aus.lb-a1b2.shop.example.com
// lb-a1b2.shop.example.com geolocation default ie.lb-a1b2.shop.example.com (set by the default geo option)
// ie.lb-a1b2.shop.example.com CNAME weighted 100 ab1.lb-a1b2.shop.example.com
// ie.lb-a1b2.shop.example.com CNAME weighted 100 aws.lb.com
// aus.lb-a1b2.shop.example.com CNAME weighted 100 ab2.lb-a1b2.shop.example.com
// aus.lb-a1b2.shop.example.com CNAME weighted 100 ab3.lb-a1b2.shop.example.com
// ab1.lb-a1b2.shop.example.com A 192.22.2.1 192.22.2.5
// ab2.lb-a1b2.shop.example.com A 192.22.2.3
// ab3.lb-a1b2.shop.example.com A 192.22.2.4

func (dh *dnsHelper) getLoadBalancedEndpoints(mcgTarget *dns.MultiClusterGatewayTarget, hostname string, currentEndpoints map[string]*v1alpha1.Endpoint) []*v1alpha1.Endpoint {

	cnameHost := hostname
	if isWildCardHost(hostname) {
		cnameHost = strings.Replace(hostname, "*.", "", -1)
	}

	var (
		endpoints       []*v1alpha1.Endpoint
		endpoint        *v1alpha1.Endpoint
		defaultEndpoint *v1alpha1.Endpoint
	)
	lbName := strings.ToLower(fmt.Sprintf("lb-%s.%s", mcgTarget.GetShortCode(), cnameHost))

	for geoCode, cgwTargets := range mcgTarget.GroupTargetsByGeo() {
		geoLbName := strings.ToLower(fmt.Sprintf("%s.%s", geoCode, lbName))
		var clusterEndpoints []*v1alpha1.Endpoint
		for _, cgwTarget := range cgwTargets {

			var ipValues []string
			var hostValues []string
			for _, gwa := range cgwTarget.Status.Addresses {
				if *gwa.Type == gatewayapiv1.IPAddressType {
					ipValues = append(ipValues, gwa.Value)
				} else {
					hostValues = append(hostValues, gwa.Value)
				}
			}

			if len(ipValues) > 0 {
				clusterLbName := strings.ToLower(fmt.Sprintf("%s.%s", cgwTarget.GetShortCode(), lbName))
				endpoint = createOrUpdateEndpoint(clusterLbName, ipValues, v1alpha1.ARecordType, "", dns.DefaultTTL, currentEndpoints)
				clusterEndpoints = append(clusterEndpoints, endpoint)
				hostValues = append(hostValues, clusterLbName)
			}

			for _, hostValue := range hostValues {
				endpoint = createOrUpdateEndpoint(geoLbName, []string{hostValue}, v1alpha1.CNAMERecordType, hostValue, dns.DefaultTTL, currentEndpoints)
				endpoint.SetProviderSpecific(dns.ProviderSpecificWeight, strconv.Itoa(cgwTarget.GetWeight()))
				clusterEndpoints = append(clusterEndpoints, endpoint)
			}
		}
		if len(clusterEndpoints) == 0 {
			continue
		}
		endpoints = append(endpoints, clusterEndpoints...)

		//Create lbName CNAME (lb-a1b2.shop.example.com -> default.lb-a1b2.shop.example.com)
		endpoint = createOrUpdateEndpoint(lbName, []string{geoLbName}, v1alpha1.CNAMERecordType, string(geoCode), dns.DefaultCnameTTL, currentEndpoints)

		//Deal with the default geo endpoint first
		if geoCode.IsDefaultCode() {
			defaultEndpoint = endpoint
			// continue here as we will add the `defaultEndpoint` later
			continue
		} else if (geoCode == mcgTarget.GetDefaultGeo()) || defaultEndpoint == nil {
			// Ensure that a `defaultEndpoint` is always set, but the expected default takes precedence
			defaultEndpoint = createOrUpdateEndpoint(lbName, []string{geoLbName}, v1alpha1.CNAMERecordType, "default", dns.DefaultCnameTTL, currentEndpoints)
		}

		endpoint.SetProviderSpecific(dns.ProviderSpecificGeoCode, string(geoCode))

		endpoints = append(endpoints, endpoint)
	}

	if len(endpoints) > 0 {
		// Add the `defaultEndpoint`, this should always be set by this point if `endpoints` isn't empty
		defaultEndpoint.SetProviderSpecific(dns.ProviderSpecificGeoCode, string(dns.WildcardGeo))
		endpoints = append(endpoints, defaultEndpoint)
		//Create gwListenerHost CNAME (shop.example.com -> lb-a1b2.shop.example.com)
		endpoint = createOrUpdateEndpoint(hostname, []string{lbName}, v1alpha1.CNAMERecordType, "", dns.DefaultCnameTTL, currentEndpoints)
		endpoints = append(endpoints, endpoint)
	}

	return endpoints
}

func createOrUpdateEndpoint(dnsName string, targets v1alpha1.Targets, recordType v1alpha1.DNSRecordType, setIdentifier string,
	recordTTL v1alpha1.TTL, currentEndpoints map[string]*v1alpha1.Endpoint) (endpoint *v1alpha1.Endpoint) {
	ok := false
	endpointID := dnsName + setIdentifier
	if endpoint, ok = currentEndpoints[endpointID]; !ok {
		endpoint = &v1alpha1.Endpoint{}
		if setIdentifier != "" {
			endpoint.SetIdentifier = setIdentifier
		}
	}
	endpoint.DNSName = dnsName
	endpoint.RecordType = string(recordType)
	endpoint.Targets = targets
	endpoint.RecordTTL = recordTTL
	return endpoint
}

// removeDNSForDeletedListeners remove any DNSRecords that are associated with listeners that no longer exist in this gateway
func (dh *dnsHelper) removeDNSForDeletedListeners(ctx context.Context, upstreamGateway *gatewayapiv1.Gateway) error {
	dnsList := &v1alpha1.DNSRecordList{}
	//List all dns records that belong to this gateway
	labelSelector := &client.MatchingLabels{
		LabelGatewayReference: upstreamGateway.Name,
	}
	if err := dh.List(ctx, dnsList, labelSelector, &client.ListOptions{Namespace: upstreamGateway.Namespace}); err != nil {
		return err
	}

	for _, dnsRecord := range dnsList.Items {
		listenerExists := false
		for _, listener := range upstreamGateway.Spec.Listeners {
			if listener.Name == gatewayapiv1.SectionName(dnsRecord.Labels[LabelListenerReference]) {
				listenerExists = true
				break
			}
		}
		if !listenerExists {
			if err := dh.Delete(ctx, &dnsRecord, &client.DeleteOptions{}); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil

}

func (dh *dnsHelper) getManagedZoneForListener(ctx context.Context, ns string, listener gatewayapiv1.Listener) (*v1alpha1.ManagedZone, error) {
	var managedZones v1alpha1.ManagedZoneList
	if err := dh.List(ctx, &managedZones, client.InNamespace(ns)); err != nil {
		log.FromContext(ctx).Error(err, "unable to list managed zones for gateway ", "in ns", ns)
		return nil, err
	}
	host := string(*listener.Hostname)
	mz, _, err := findMatchingManagedZone(host, host, managedZones.Items)
	return mz, err
}

func dnsRecordName(gatewayName, listenerName string) string {
	return fmt.Sprintf("%s-%s", gatewayName, listenerName)
}

func (dh *dnsHelper) createDNSRecordForListener(ctx context.Context, gateway *gatewayapiv1.Gateway, dnsPolicy *v1alpha1.DNSPolicy, mz *v1alpha1.ManagedZone, listener gatewayapiv1.Listener) (*v1alpha1.DNSRecord, error) {
	logger := log.FromContext(ctx)
	logger.Info("creating dns for gateway listener", "listener", listener.Name)
	dnsRecord := dh.buildDNSRecordForListener(gateway, dnsPolicy, listener, mz)
	if err := controllerutil.SetControllerReference(mz, dnsRecord, dh.Scheme()); err != nil {
		return dnsRecord, err
	}

	err := dh.Create(ctx, dnsRecord, &client.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return dnsRecord, err
	}
	if err != nil && k8serrors.IsAlreadyExists(err) {
		err = dh.Get(ctx, client.ObjectKeyFromObject(dnsRecord), dnsRecord)
		if err != nil {
			return dnsRecord, err
		}
	}
	return dnsRecord, nil
}

func (dh *dnsHelper) deleteDNSRecordForListener(ctx context.Context, owner metav1.Object, listener gatewayapiv1.Listener) error {
	recordName := dnsRecordName(owner.GetName(), string(listener.Name))
	dnsRecord := v1alpha1.DNSRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recordName,
			Namespace: owner.GetNamespace(),
		},
	}
	return dh.Delete(ctx, &dnsRecord, &client.DeleteOptions{})
}

func isWildCardHost(host string) bool {
	return strings.HasPrefix(host, "*")
}

func (dh *dnsHelper) getDNSHealthCheckProbes(ctx context.Context, gateway *gatewayapiv1.Gateway, dnsPolicy *v1alpha1.DNSPolicy) ([]*v1alpha1.DNSHealthCheckProbe, error) {
	list := &v1alpha1.DNSHealthCheckProbeList{}
	if err := dh.List(ctx, list, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(commonDNSRecordLabels(client.ObjectKeyFromObject(gateway), client.ObjectKeyFromObject(dnsPolicy))),
		Namespace:     dnsPolicy.Namespace,
	}); err != nil {
		return nil, err
	}

	return slice.MapErr(list.Items, func(obj v1alpha1.DNSHealthCheckProbe) (*v1alpha1.DNSHealthCheckProbe, error) {
		return &obj, nil
	})
}
