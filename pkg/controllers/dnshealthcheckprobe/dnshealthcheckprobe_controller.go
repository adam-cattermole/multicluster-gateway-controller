package dnshealthcheckprobe

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/health"
)

const (
	DNSHealthCheckProbeFinalizer = "kuadrant.io/dns-health-check-probe"
)

var (
	ErrInvalidHeader = fmt.Errorf("invalid header format")
)

type DNSHealthCheckProbeReconciler struct {
	client.Client
	HealthMonitor *health.Monitor
	Queue         *health.QueuedProbeWorker
}

// +kubebuilder:rbac:groups=kuadrant.io,resources=dnshealthcheckprobes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuadrant.io,resources=dnshealthcheckprobes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuadrant.io,resources=dnshealthcheckprobes/finalizers,verbs=get;update;patch

func (r *DNSHealthCheckProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	previous := &v1alpha1.DNSHealthCheckProbe{}
	err := r.Client.Get(ctx, req.NamespacedName, previous)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	logger.V(3).Info("DNSHealthCheckProbeReconciler Reconcile", "dnsHealthCheckProbe", previous)

	probeObj := previous.DeepCopy()

	if probeObj.DeletionTimestamp != nil && !probeObj.DeletionTimestamp.IsZero() {
		logger.Info("deleting probe", "probe", probeObj)

		r.deleteProbe(probeObj)
		controllerutil.RemoveFinalizer(probeObj, DNSHealthCheckProbeFinalizer)

		if err := r.Update(ctx, probeObj); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(probeObj, DNSHealthCheckProbeFinalizer) {
		controllerutil.AddFinalizer(probeObj, DNSHealthCheckProbeFinalizer)
		if err := r.Update(ctx, probeObj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set the interval
	interval := probeObj.Spec.Interval.Duration

	// Set the protocol: default to HTTP is not defined
	protocol := probeObj.Spec.Protocol
	if protocol == "" {
		protocol = v1alpha1.HttpProtocol
	}

	protocol = v1alpha1.NewHealthProtocol(string(probeObj.Spec.Protocol))

	probeId := probeId(probeObj)

	additionalHeaders, err := getAdditionalHeaders(ctx, r.Client, probeObj)

	if err != nil {
		//update probe status, ignore update errors
		_ = r.Client.Status().Update(ctx, probeObj)
		return ctrl.Result{}, err
	}

	if r.HealthMonitor.HasProbe(probeId) {
		r.HealthMonitor.UpdateProbe(probeId, func(p *health.ProbeQueuer) {
			p.Interval = interval
			p.Host = probeObj.Spec.Host
			p.IPAddress = probeObj.Spec.Address
			p.Path = probeObj.Spec.Path
			p.Port = probeObj.Spec.Port
			p.Protocol = protocol
			p.AdditionalHeaders = additionalHeaders
			p.ExpectedResponses = probeObj.Spec.ExpectedResponses
			p.AllowInsecureCertificate = probeObj.Spec.AllowInsecureCertificate
		})
	} else {
		notifier := NewStatusUpdateProbeNotifier(r.Client, previous)
		r.HealthMonitor.AddProbeQueuer(&health.ProbeQueuer{
			ID:                       probeId,
			Interval:                 interval,
			Host:                     probeObj.Spec.Host,
			Path:                     probeObj.Spec.Path,
			Port:                     probeObj.Spec.Port,
			Protocol:                 protocol,
			IPAddress:                probeObj.Spec.Address,
			AdditionalHeaders:        additionalHeaders,
			ExpectedResponses:        probeObj.Spec.ExpectedResponses,
			AllowInsecureCertificate: probeObj.Spec.AllowInsecureCertificate,
			Notifier:                 notifier,
			Queue:                    r.Queue,
		})
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the manager
func (r *DNSHealthCheckProbeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DNSHealthCheckProbe{}).
		Complete(r)
}

func (r *DNSHealthCheckProbeReconciler) deleteProbe(probeObj *v1alpha1.DNSHealthCheckProbe) {
	r.HealthMonitor.RemoveProbe(probeId(probeObj))
}

func probeId(probeObj *v1alpha1.DNSHealthCheckProbe) string {
	return fmt.Sprintf("%s/%s", probeObj.Namespace, probeObj.Name)
}

func getAdditionalHeaders(ctx context.Context, clt client.Client, probeObj *v1alpha1.DNSHealthCheckProbe) (v1alpha1.AdditionalHeaders, error) {
	additionalHeaders := v1alpha1.AdditionalHeaders{}

	if probeObj.Spec.AdditionalHeadersRef != nil {
		secretKey := client.ObjectKey{Name: probeObj.Spec.AdditionalHeadersRef.Name, Namespace: probeObj.Namespace}
		additionalHeadersSecret := &v1.Secret{}
		if err := clt.Get(ctx, secretKey, additionalHeadersSecret); client.IgnoreNotFound(err) != nil {
			return additionalHeaders, fmt.Errorf("unusual error retrieving additional headers secret %v/%v: %w", secretKey.Namespace, secretKey.Name, err)
		} else if err != nil {
			probeError := fmt.Errorf("error retrieving additional headers secret %v/%v: %w", secretKey.Namespace, secretKey.Name, err)
			probeObj.Status.Healthy = false
			probeObj.Status.ConsecutiveFailures = 0
			probeObj.Status.Reason = "additional headers secret not found"
			return additionalHeaders, probeError
		}
		for k, v := range additionalHeadersSecret.Data {
			if strings.ContainsAny(strings.TrimSpace(k), " \t") {
				probeObj.Status.Healthy = false
				probeObj.Status.ConsecutiveFailures = 0
				probeObj.Status.Reason = "invalid header found: " + k
				return nil, fmt.Errorf("invalid header, must not contain whitespace '%v': %w", k, ErrInvalidHeader)
			}
			additionalHeaders = append(additionalHeaders, v1alpha1.AdditionalHeader{
				Name:  strings.TrimSpace(k),
				Value: string(v),
			})
		}
	}
	return additionalHeaders, nil
}
