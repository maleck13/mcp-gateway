package controller

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	mcpv1alpha1 "github.com/Kuadrant/mcp-gateway/api/v1alpha1"
	"github.com/Kuadrant/mcp-gateway/internal/broker/upstream"
	"github.com/Kuadrant/mcp-gateway/internal/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (

	// CredentialSecretLabel is the required label for credential secrets
	CredentialSecretLabel = "mcp.kagenti.com/credential" //nolint:gosec // not a credential, just a label name
	// CredentialSecretValue is the required value for credential secrets
	CredentialSecretValue = "true"
	// HTTPRouteIndex used to find MCPServerRegistrations
	HTTPRouteIndex = "spec.targetRef.httproute"
	// HTTPRouteIndex used to find programmed httproutes TODO do we still need this?
	ProgrammedHTTPRouteIndex = "status.hasProgrammedCondition"
)

// getConfigNamespace returns the namespace for config, using NAMESPACE env var or defaulting to mcp-system
func getConfigNamespace() string {
	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "mcp-system"
	}
	return namespace
}

// ServerInfo holds server information
type ServerInfo struct {
	ID                 string
	Endpoint           string
	Hostname           string
	ToolPrefix         string
	HTTPRouteName      string
	HTTPRouteNamespace string
	Credential         string
}

type MCPServerConfigReaderWriter interface {
	WriteMCPServerConfig(ctx context.Context, servers []config.MCPServer, namespaceName types.NamespacedName) error
	WriteEmptyConfig(ctx context.Context, namespaceName types.NamespacedName) error
	UpsertMCPServer(ctx context.Context, server config.MCPServer, namespaceName types.NamespacedName) error
	// RemoveMCPServer removes a server from all config secrets cluster-wide
	RemoveMCPServer(ctx context.Context, serverName string) error
}

// MCPReconciler reconciles both MCPServerRegistration and MCPVirtualServer resources
type MCPReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	DirectAPIReader    client.Reader // uncached reader for fetching secrets
	ConfigReaderWriter MCPServerConfigReaderWriter
}

// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpserverregistrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpserverregistrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpvirtualservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// TODOS
// - should we make target ref immutable we need to handle this changing (it doesn't currently)

// Reconcile reconciles both MCPServerRegistration and MCPVirtualServer resources
func (r *MCPReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := logf.FromContext(ctx).WithValues("resource", "mcpserverregistration")
	logger.V(1).Info("Reconciling", "mcpregistrationname", req.Name, "namespace", req.Namespace)

	mcpsr := &mcpv1alpha1.MCPServerRegistration{}
	if err := r.Get(ctx, req.NamespacedName, mcpsr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("found", "mcpregistrationname", mcpsr.Name, "namespace", mcpsr.Namespace)

	// handle deletion
	if !mcpsr.DeletionTimestamp.IsZero() {
		logger.Info("deleting", "mcpregistrationname", mcpsr.Name, "namespace", mcpsr.Namespace)
		if controllerutil.ContainsFinalizer(mcpsr, mcpGatewayFinalizer) {
			// todo we need gateway namespace for httproute
			if err := r.ConfigReaderWriter.RemoveMCPServer(ctx, mcpServerName(mcpsr)); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(mcpsr, mcpGatewayFinalizer)
			if err := r.Update(ctx, mcpsr); err != nil {
				if errors.IsConflict(err) {
					logger.V(1).Info("conflict err requing to retry")
					return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
				}
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	// add finalizer if not present
	if !controllerutil.ContainsFinalizer(mcpsr, mcpGatewayFinalizer) {
		logger.V(1).Info("no finalizer adding", "mcpregistrationname", mcpsr.Name)
		if controllerutil.AddFinalizer(mcpsr, mcpGatewayFinalizer) {
			if err := r.Update(ctx, mcpsr); err != nil {
				if errors.IsConflict(err) {
					logger.V(1).Info("conflict err requing to retry")
					return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
				}
				return ctrl.Result{}, err
			}
			logger.V(1).Info("finalizer added", "mcpregistrationname", mcpsr.Name)
			return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
		}
	}
	logger.Info("main reconcile logic starting for", "mcpregistrationname", mcpsr.Name)

	//get the httproute and gateway(s) this mcpserverregistration is targeting
	targetRoute, err := r.getTargetHTTPRoute(ctx, mcpsr)
	if err != nil {
		if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
			if errors.IsConflict(err) {
				// don't log these as they are just noise
				return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
			}
			return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
		}
		return ctrl.Result{}, fmt.Errorf("reconcile failed %w", err)
	}
	logger.Info("target route found ", "mcpregistrationname", targetRoute.Name)

	// find gateways that have accepted the httproute
	validGateways, err := r.findValidGatewaysForMCPServer(ctx, targetRoute)
	if err != nil {
		if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
			if errors.IsConflict(err) {
				// don't log these as they are just noise
				return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
			}
			return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
		}
		return ctrl.Result{}, fmt.Errorf("reconcile failed %w", err)
	}

	// no valid gateways found. Unexpected lets exit
	if len(validGateways) == 0 {
		err := fmt.Errorf("no valid gateways for httproute")
		logger.Error(err, "failed to find any valid gateways", "route", targetRoute)
		if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
			if errors.IsConflict(err) {
				// don't log these as they are just noise
				return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
			}
			return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
		}
		return ctrl.Result{}, fmt.Errorf("reconcile failed %w", err)
	}
	logger.Info("valid gateways discovered ", "total", len(validGateways), "mcpregistrationname", mcpsr.Name)
	// is there a valid mcpgatewayextension
	validNamespaces := []string{}
	for _, vg := range validGateways {
		validMcpGatewayExtensions, err := r.findValidMCPGatewayExtsForGateway(ctx, vg)
		if err != nil {
			logger.Error(err, "failed to find valid mcpgatewayextension ", "gateway", vg, "mcpserverregistration", mcpsr)
			if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
				if errors.IsConflict(err) {
					// don't log these as they are just noise
					return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
				}
				return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
			}
		}
		if len(validMcpGatewayExtensions) == 0 {
			// this is not an error so we are going to exit
			if err := r.updateStatus(ctx, mcpsr, false, "no valid mcpgatewayextensions configured", 0); err != nil {
				if errors.IsConflict(err) {
					// don't log these as they are just noise
					return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
				}
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		for _, vext := range validMcpGatewayExtensions {
			validNamespaces = append(validNamespaces, vext.Namespace)
		}
	}

	mcpServerconfig, err := r.buildMCPServerConfig(ctx, targetRoute, mcpsr)
	if err != nil {
		if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
			if errors.IsConflict(err) {
				// don't log these as they are just noise
				return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
			}
			return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
		}
		return reconcile.Result{}, fmt.Errorf("failed to reconcile %s %w", mcpsr.Name, err)
	}
	for _, configNs := range validNamespaces {
		if err := r.ConfigReaderWriter.UpsertMCPServer(ctx, *mcpServerconfig, config.ConfigNamespaceName(configNs)); err != nil {
			if err := r.updateStatus(ctx, mcpsr, false, err.Error(), 0); err != nil {
				if errors.IsConflict(err) {
					// don't log these as they are just noise
					return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
				}
				return ctrl.Result{}, fmt.Errorf("reconcile failed: status update failed %w", err)
			}
			return reconcile.Result{}, fmt.Errorf("failed to reconcile %s %w", mcpsr.Name, err)
		}
	}

	// Everything is in place now so we will now poll the gateway to check the registration status of the mcpserver
	if err := r.setMCPServerRegistrationStatus(ctx, mcpsr, string(mcpServerconfig.ID())); err != nil {
		if strings.Contains(err.Error(), "mcp server is not present in gateway yet") {
			logger.V(1).Info("config not loaded in gateway yet. Will retry status check", "mcpserverregistration", mcpsr.Name)
			// no point hammering the gateway when we know we are waiting for the config to be loaded
			return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(err, "failed to set mcpserverregistration status", "mcpserverregistration", mcpsr.Name)
		// TODO handle case where this keep failing forever we can prob requeue on specific err type
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil

}

// todo this should be shared with the broker
func mcpServerName(mcp *mcpv1alpha1.MCPServerRegistration) string {
	return fmt.Sprintf(
		"%s/%s",
		mcp.Namespace,
		mcp.Name,
	)
}

// TODO these duplicates code from the mcpgatewayextension reconcile. Deduplicate
// findValidMCPGatewayExtsForGateway will find all MCPGatewayExtensions indexed against passed Gateway instance
func (r *MCPReconciler) findValidMCPGatewayExtsForGateway(ctx context.Context, g *gatewayv1.Gateway) ([]*mcpv1alpha1.MCPGatewayExtension, error) {
	logger := logf.FromContext(ctx).WithName("findValidMCPGatewayExtsForGateway")
	validExtensions := []*mcpv1alpha1.MCPGatewayExtension{}
	mcpGatewayExtList := &mcpv1alpha1.MCPGatewayExtensionList{}
	if err := r.List(ctx, mcpGatewayExtList,
		client.MatchingFields{gatewayIndexKey: gatewayToMCPExtIndexValue(*g)},
	); err != nil {
		return validExtensions, err
	}
	logger.V(1).Info("found mcpgatewayextensions", "total", len(mcpGatewayExtList.Items))
	for _, mg := range mcpGatewayExtList.Items {

		if mg.Namespace == g.Namespace {
			validExtensions = append(validExtensions, &mg)
			continue
		}
		has, err := r.hasValidReferenceGrant(ctx, &mg)
		if err != nil {
			// we have to exit here
			return validExtensions, fmt.Errorf("failed to check if mcpgatewayextension is valid %w", err)
		}
		if has && meta.IsStatusConditionTrue(mg.Status.Conditions, mcpv1alpha1.ConditionTypeReady) {
			validExtensions = append(validExtensions, &mg)
		}
	}
	// next we need to check is this mcpgateway in the same namespace as the gateway if it is it is valid
	// we also need to check if there is a reference grant in the namespace
	return validExtensions, nil
}

// hasValidReferenceGrant checks if a valid ReferenceGrant exists that allows the MCPGatewayExtension
// to reference a Gateway in a different namespace
func (r *MCPReconciler) hasValidReferenceGrant(ctx context.Context, mcpExt *mcpv1alpha1.MCPGatewayExtension) (bool, error) {
	// list ReferenceGrants in the target Gateway's namespace
	refGrantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := r.List(ctx, refGrantList, client.InNamespace(mcpExt.Spec.TargetRef.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list ReferenceGrants: %w", err)
	}

	for _, rg := range refGrantList.Items {
		if r.referenceGrantAllows(&rg, mcpExt) {
			return true, nil
		}
	}
	return false, nil
}

// referenceGrantAllows checks if a ReferenceGrant permits the MCPGatewayExtension to reference a Gateway
func (r *MCPReconciler) referenceGrantAllows(rg *gatewayv1beta1.ReferenceGrant, mcpExt *mcpv1alpha1.MCPGatewayExtension) bool {
	fromAllowed := false
	toAllowed := false

	// check if 'from' allows MCPGatewayExtension from its namespace
	for _, from := range rg.Spec.From {
		if string(from.Group) == mcpv1alpha1.GroupVersion.Group &&
			string(from.Kind) == "MCPGatewayExtension" &&
			string(from.Namespace) == mcpExt.Namespace {
			fromAllowed = true
			break
		}
	}

	if !fromAllowed {
		return false
	}

	// check if 'to' allows Gateway references
	for _, to := range rg.Spec.To {
		// empty group means core, but Gateway is in gateway.networking.k8s.io
		if string(to.Group) == gatewayv1.GroupVersion.Group {
			// empty kind means all kinds in the group, or specific Gateway kind
			if to.Kind == "" || string(to.Kind) == "Gateway" {
				// if name is specified, it must match; empty means all
				if to.Name == nil || *to.Name == "" || string(*to.Name) == mcpExt.Spec.TargetRef.Name {
					toAllowed = true
					break
				}
			}
		}
	}

	return toAllowed
}

// findValidGatewaysForMCPServer returns the gateways the httproute targeted by the MCPServerRegistration is the child of
func (r *MCPReconciler) findValidGatewaysForMCPServer(ctx context.Context, targetHTTPRoute *gatewayv1.HTTPRoute) ([]*gatewayv1.Gateway, error) {
	logger := logf.FromContext(ctx).WithName("findValidGatewaysForMCPServer")
	var validGateways = []*gatewayv1.Gateway{}
	if len(targetHTTPRoute.Status.Parents) > 0 {
		// lets fetch the parents that are valid
		for _, parent := range targetHTTPRoute.Status.Parents {
			acceptedCond := meta.FindStatusCondition(parent.Conditions, string(gatewayv1.GatewayConditionAccepted))
			if acceptedCond.Status == metav1.ConditionTrue {
				pr := parent.ParentRef.DeepCopy()
				if pr.Namespace == nil {
					pr.Namespace = ptr.To(gatewayv1.Namespace(targetHTTPRoute.Namespace))
				}
				pg, err := r.getTargetGatewaysFromParentRef(ctx, pr)
				if err != nil {
					if errors.IsNotFound(err) {
						// log but continue we will handle no gateways found later
						logger.Error(err, "failed to find parent for httproute")
						continue
					}
					//unexpected error
					return validGateways, fmt.Errorf("unexpected error getting gateway from httproute %w", err)
				}
				// as this httproute was accepted by the gateway lets add it valid gateways
				validGateways = append(validGateways, pg)
			}
		}
	}
	return validGateways, nil
}

func (r *MCPReconciler) getTargetHTTPRoute(ctx context.Context, mcpsr *mcpv1alpha1.MCPServerRegistration) (*gatewayv1.HTTPRoute, error) {
	namespaceName := types.NamespacedName{Namespace: mcpsr.Namespace, Name: mcpsr.Spec.TargetRef.Name}
	logger := logf.FromContext(ctx).WithValues("method", "getTargetHTTPRoute")
	logger.V(1).Info("httproute target ", "namespacename ", namespaceName)
	targetRoute := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, namespaceName, targetRoute); err != nil {
		return nil, fmt.Errorf("failed to get targetted httproute %w", err)
	}
	return targetRoute, nil

}

func (r *MCPReconciler) getTargetGatewaysFromParentRef(ctx context.Context, parent *gatewayv1.ParentReference) (*gatewayv1.Gateway, error) {
	namespaceName := types.NamespacedName{Namespace: string(*parent.Namespace), Name: string(parent.Name)}
	g := &gatewayv1.Gateway{}
	if err := r.Client.Get(ctx, namespaceName, g); err != nil {
		return nil, fmt.Errorf("failed to get parent gateway for httproute: %w", err)
	}
	return g, nil
}

// reconcileMCPServerRegistration handles MCPServerRegistration reconciliation (existing logic)
func (r *MCPReconciler) setMCPServerRegistrationStatus(ctx context.Context, mcpsr *mcpv1alpha1.MCPServerRegistration, serverID string) error {
	log := logf.FromContext(ctx)
	log.V(1).Info("setMCPServerRegistrationStatus", "mcpregistrationname", mcpsr.Name, "namespace", mcpsr.Namespace)

	validator := NewServerValidator(r.Client)
	// TODO this currently lists all servers
	statusResponse, err := validator.ValidateServers(ctx)
	if err != nil {
		log.Error(err, "Failed to validate server status via broker")
		ready, message := false, fmt.Sprintf("Validation failed: %v", err)
		if err := r.updateStatus(ctx, mcpsr, ready, message, 0); err != nil {
			log.Error(err, "Failed to update status")
			return err
		}
		return err
	}

	var gatewayServerStatus upstream.ServerValidationStatus
	for _, sr := range statusResponse.Servers {
		log.Info("server response ", "id", sr.ID, "controller id ", serverID)
		if sr.ID == serverID {
			gatewayServerStatus = sr
			break
		}
	}

	log.Info("server status ", "mcpregistrationname", mcpsr.Name, "status", gatewayServerStatus)
	message := "mcp server is not present in gateway yet"
	// if there is an id that matches then the gateway is registering the mcp
	if gatewayServerStatus.ID != "" {
		if err := r.updateStatus(ctx, mcpsr, gatewayServerStatus.Ready, gatewayServerStatus.Message, gatewayServerStatus.TotalTools); err != nil {
			log.Error(err, "Failed to update status")
			return err
		}

		if err := r.updateHTTPRouteStatus(ctx, mcpsr, true); err != nil {
			log.Error(err, "Failed to update HTTPRoute status")
		}
		if !gatewayServerStatus.Ready {
			return fmt.Errorf(message)
		}
		log.V(1).Info("server is ready")
		return nil
	}
	// otherwise it hasn't picked up the config yet

	if err := r.updateStatus(ctx, mcpsr, gatewayServerStatus.Ready, message, 0); err != nil {
		return err
	}

	return fmt.Errorf(message)
}

func (r *MCPReconciler) buildMCPServerConfig(ctx context.Context, targetRoute *gatewayv1.HTTPRoute, mcpsr *mcpv1alpha1.MCPServerRegistration) (*config.MCPServer, error) {
	if mcpsr.DeletionTimestamp != nil {
		// don't add deleting mcpserver
		return nil, fmt.Errorf("cant generate config for deleting server %s/%s", mcpsr.Namespace, mcpsr.Name)
	}
	serverInfo, err := r.buildServerInfoFromHTTPRoute(ctx, targetRoute, mcpsr.Spec.Path)
	// TODO: implement
	serverName := mcpServerName(mcpsr)
	serverConfig := config.MCPServer{
		Name:       serverName,
		URL:        serverInfo.Endpoint,
		Hostname:   serverInfo.Hostname,
		ToolPrefix: mcpsr.Spec.ToolPrefix,
		Enabled:    true,
	}

	// add credential env var if configured
	if mcpsr.Spec.CredentialRef != nil {
		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{
			Name:      mcpsr.Spec.CredentialRef.Name,
			Namespace: mcpsr.Namespace,
		}, secret)
		if err != nil {
			return nil, fmt.Errorf("failed to read credential ref sercret when building config %w", err)
		}
		val, ok := secret.Data[mcpsr.Spec.CredentialRef.Key]
		if !ok {
			return nil, fmt.Errorf("the secret no credentialref key field %s present %w", mcpsr.Spec.CredentialRef.Key, err)
		}
		serverConfig.Credential = string(val)

	}
	return &serverConfig, nil
}

// func (r *MCPReconciler) regenerateAggregatedConfig(ctx context.Context) error {
// 	log := logf.FromContext(ctx)
// 	// TODO this currently regenerates the entire config
// 	mcpsrList := &mcpv1alpha1.MCPServerRegistrationList{}
// 	if err := r.List(ctx, mcpsrList); err != nil {
// 		return fmt.Errorf("failed to regenerateAggregatedConfig %w", err)
// 	}

// 	referencedHTTPRoutes := make(map[string]struct{})
// 	for _, mcpsr := range mcpsrList.Items {
// 		targetRef := mcpsr.Spec.TargetRef
// 		if targetRef.Kind == "HTTPRoute" {
// 			namespace := mcpsr.Namespace
// 			if targetRef.Namespace != "" {
// 				namespace = targetRef.Namespace
// 			}
// 			key := fmt.Sprintf("%s/%s", namespace, targetRef.Name)
// 			referencedHTTPRoutes[key] = struct{}{}
// 		}
// 	}
// 	servers := []config.MCPServer{}
// 	if len(mcpsrList.Items) == 0 {
// 		log.Info("No MCPServerRegistrations found, writing empty ConfigMap")
// 		if err := r.ConfigReaderWriter.WriteMCPServerConfig(ctx, servers, config.DefaultNamespaceName); err != nil {
// 			return fmt.Errorf("Failed to write empty configuration %w", err)
// 		}
// 		log.Info("Successfully wrote empty ConfigMap")
// 		return nil
// 	}

// 	for _, mcpsr := range mcpsrList.Items {
// 		if mcpsr.DeletionTimestamp != nil {
// 			// don't add deleting mcpserver
// 			continue
// 		}
// 		httpRoute, err := r.getTargetHTTPRoute(ctx, &mcpsr)
// 		// TODO we need the httproute here
// 		serverInfo, err := r.buildServerInfoFromHTTPRoute(ctx, httpRoute, mcpsr.Spec.Path)
// 		if err != nil {
// 			log.Error(err, "Failed to discover server endpoints",
// 				"mcpregistrationname", mcpsr.Name,
// 				"namespace", mcpsr.Namespace)
// 			continue
// 		}

// 		serverName := fmt.Sprintf(
// 			"%s/%s",
// 			serverInfo.HTTPRouteNamespace,
// 			serverInfo.HTTPRouteName,
// 		)
// 		serverConfig := config.MCPServer{
// 			Name:       serverName,
// 			URL:        serverInfo.Endpoint,
// 			Hostname:   serverInfo.Hostname,
// 			ToolPrefix: mcpsr.Spec.ToolPrefix,
// 			Enabled:    true,
// 		}

// 		// add credential env var if configured
// 		if mcpsr.Spec.CredentialRef != nil {
// 			secret := &corev1.Secret{}
// 			err = r.Get(ctx, types.NamespacedName{
// 				Name:      mcpsr.Spec.CredentialRef.Name,
// 				Namespace: mcpsr.Namespace,
// 			}, secret)
// 			if err != nil {
// 				log.Error(err, "failed to read credential secret")
// 				continue
// 			}
// 			val, ok := secret.Data[mcpsr.Spec.CredentialRef.Key]
// 			if !ok {
// 				// no key log and continue
// 				log.V(1).Info("the secret had no key ", "specified key", mcpsr.Spec.CredentialRef.Key)
// 				continue
// 			}
// 			serverConfig.Credential = string(val)

// 		}

// 		servers = append(servers, serverConfig)

// 	}

// 	if err := r.ConfigReaderWriter.WriteMCPServerConfig(ctx, servers, config.DefaultNamespaceName); err != nil {

// 		log.Error(err, "Failed to write aggregated configuration")
// 		return err
// 	}

// 	log.V(1).Info("Successfully regenerated aggregated configuration", "serverCount", len(servers))

// 	if err := r.cleanupOrphanedHTTPRoutes(ctx, referencedHTTPRoutes); err != nil {
// 		log.Error(err, "Failed to cleanup orphaned HTTPRoute conditions")
// 		return err
// 	}

// 	return nil
// }

func (r *MCPReconciler) buildServerInfoFromHTTPRoute(ctx context.Context, httpRoute *gatewayv1.HTTPRoute, path string) (*ServerInfo, error) {

	if len(httpRoute.Spec.Rules) == 0 || len(httpRoute.Spec.Rules[0].BackendRefs) == 0 {
		return nil, fmt.Errorf("HTTPRoute %s/%s has no backend references", httpRoute.Namespace, httpRoute.Name)
	}

	if len(httpRoute.Spec.Rules) > 1 {
		return nil, fmt.Errorf(
			"HTTPRoute %s/%s has > 1 rule, which is unsupported", httpRoute.Namespace, httpRoute.Name)
	}

	if len(httpRoute.Spec.Rules[0].BackendRefs) > 1 {
		return nil, fmt.Errorf("HTTPRoute %s/%s has > 1 backend reference, which is unsupported", httpRoute.Namespace, httpRoute.Name)
	}

	backendRef := httpRoute.Spec.Rules[0].BackendRefs[0]
	if backendRef.Name == "" {
		return nil, fmt.Errorf("HTTPRoute %s/%s backend reference has no name", httpRoute.Namespace, httpRoute.Name)
	}

	kind := "Service"
	if backendRef.Kind != nil {
		kind = string(*backendRef.Kind)
	}

	group := ""
	if backendRef.Group != nil {
		group = string(*backendRef.Group)
	}

	// handle Istio Hostname backendRef for external services
	if kind == "Hostname" && group == "networking.istio.io" {
		log.FromContext(ctx).V(1).Info("processing external service via Hostname backendRef", "host", backendRef.Name)
		return r.buildServerInfoForHostnameBackend(httpRoute, backendRef, path)
	}

	if kind != "Service" {
		return nil, fmt.Errorf("backend reference is not a Service: %s", kind)
	}

	// Determine service namespace, default to HTTPRoute namespace
	serviceNamespace := httpRoute.Namespace
	if backendRef.Namespace != nil {
		serviceNamespace = string(*backendRef.Namespace)
	}

	// check service type
	backendName := string(backendRef.Name)
	var nameAndEndpoint string
	isExternal := false

	service := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      backendName,
		Namespace: serviceNamespace,
	}, service)

	if err != nil {
		return nil, fmt.Errorf("failed to get service %s: %w", backendName, err)
	}

	// Extract hostname from HTTPRoute
	if len(httpRoute.Spec.Hostnames) == 0 {
		return nil, fmt.Errorf(
			"HTTPRoute %s/%s must have at least one hostname for MCP backend routing", httpRoute.Namespace, httpRoute.Name)
	}
	// use first hostname if multiple are present
	hostname := string(httpRoute.Spec.Hostnames[0])

	if service.Spec.Type == corev1.ServiceTypeExternalName {
		// externalname service points to external host
		isExternal = true
		externalName := service.Spec.ExternalName
		if backendRef.Port != nil {
			nameAndEndpoint = fmt.Sprintf("%s:%d", externalName, *backendRef.Port)
		} else {
			nameAndEndpoint = externalName
		}
	} else {
		// regular k8s service
		serviceDNSName := fmt.Sprintf("%s.%s.svc.cluster.local", backendRef.Name, serviceNamespace)
		if backendRef.Port != nil {
			nameAndEndpoint = fmt.Sprintf("%s:%d", serviceDNSName, *backendRef.Port)
		} else {
			nameAndEndpoint = serviceDNSName
		}
	}

	protocol := "http"
	if httpRoute.Spec.ParentRefs != nil {
		for _, parentRef := range httpRoute.Spec.ParentRefs {
			if parentRef.SectionName != nil &&
				strings.Contains(string(*parentRef.SectionName), "https") {
				protocol = "https"
				break
			}
		}
	}

	// determine protocol for external services
	if isExternal {
		// use appProtocol from Service spec (standard k8s field)
		for _, port := range service.Spec.Ports {
			if backendRef.Port != nil && port.Port == *backendRef.Port {
				if port.AppProtocol != nil && strings.ToLower(*port.AppProtocol) == "https" {
					protocol = "https"
				}
				break
			}
		}
	}

	endpoint := fmt.Sprintf("%s://%s%s", protocol, nameAndEndpoint, path)

	routingHostname := hostname
	if isExternal {
		// extract hostname without port
		if idx := strings.LastIndex(nameAndEndpoint, ":"); idx != -1 {
			routingHostname = nameAndEndpoint[:idx]
		} else {
			routingHostname = nameAndEndpoint
		}
	}

	serverInfo := ServerInfo{
		Endpoint:           endpoint,
		Hostname:           routingHostname,
		HTTPRouteName:      httpRoute.Name,
		HTTPRouteNamespace: httpRoute.Namespace,
		Credential:         "",
	}
	return &serverInfo, nil
}

// buildServerInfoForHostnameBackend handles Istio Hostname backendRef for external services
func (r *MCPReconciler) buildServerInfoForHostnameBackend(
	httpRoute *gatewayv1.HTTPRoute,
	backendRef gatewayv1.HTTPBackendRef,
	path string,
) (*ServerInfo, error) {
	namespace := httpRoute.Namespace
	httpRouteName := httpRoute.Name
	if len(httpRoute.Spec.Hostnames) == 0 {
		return nil, fmt.Errorf("HTTPRoute %s/%s must have at least one hostname", namespace, httpRouteName)
	}

	externalHost := string(backendRef.Name)
	if !isValidHostname(externalHost) {
		return nil, fmt.Errorf("invalid hostname in backendRef: %s", externalHost)
	}

	port := "443"
	if backendRef.Port != nil {
		port = fmt.Sprintf("%d", *backendRef.Port)
	}

	endpoint := fmt.Sprintf("https://%s%s", net.JoinHostPort(externalHost, port), path)
	routingHostname := string(httpRoute.Spec.Hostnames[0])

	return &ServerInfo{
		Endpoint:           endpoint,
		Hostname:           routingHostname,
		HTTPRouteName:      httpRouteName,
		HTTPRouteNamespace: namespace,
		Credential:         "",
	}, nil
}

// validates hostname can't contain path injection
func isValidHostname(hostname string) bool {
	if hostname == "" {
		return false
	}
	u, err := url.Parse("//" + hostname)
	if err != nil {
		return false
	}
	return u.Host == hostname
}

func (r *MCPReconciler) cleanupOrphanedHTTPRoutes(ctx context.Context, referencedHTTPRoutes map[string]struct{}) error {
	log := log.FromContext(ctx)

	httpRouteList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRouteList, client.MatchingFields{ProgrammedHTTPRouteIndex: "true"}); err != nil {
		return fmt.Errorf("failed to list programmed HTTPRoutes: %w", err)
	}

	for _, httpRoute := range httpRouteList.Items {
		key := fmt.Sprintf("%s/%s", httpRoute.Namespace, httpRoute.Name)

		if _, referenced := referencedHTTPRoutes[key]; referenced {
			continue
		}

		hasProgrammedCondition := false
		updateNeeded := false
		for i, parentStatus := range httpRoute.Status.Parents {
			newConditions := []metav1.Condition{}
			for _, condition := range parentStatus.Conditions {
				if condition.Type == "Programmed" && condition.Status == metav1.ConditionTrue {
					hasProgrammedCondition = true
					updateNeeded = true
				} else {
					newConditions = append(newConditions, condition)
				}
			}
			if updateNeeded {
				httpRoute.Status.Parents[i].Conditions = newConditions
			}
		}

		if hasProgrammedCondition {
			log.Info("Cleaning up Programmed condition on orphaned HTTPRoute",
				"HTTPRoute", httpRoute.Name,
				"namespace", httpRoute.Namespace)

			if err := r.Status().Update(ctx, &httpRoute); err != nil {
				log.Error(err, "Failed to cleanup HTTPRoute status",
					"HTTPRoute", httpRoute.Name,
					"namespace", httpRoute.Namespace)
			}
		}
	}

	return nil
}

func (r *MCPReconciler) updateHTTPRouteStatus(
	ctx context.Context,
	mcpsr *mcpv1alpha1.MCPServerRegistration,
	affected bool,
) error {
	log := log.FromContext(ctx)
	targetRef := mcpsr.Spec.TargetRef

	if targetRef.Kind != "HTTPRoute" {
		return nil
	}

	namespace := mcpsr.Namespace
	if targetRef.Namespace != "" {
		namespace = targetRef.Namespace
	}

	httpRoute := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      targetRef.Name,
		Namespace: namespace,
	}, httpRoute)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get HTTPRoute: %w", err)
	}

	condition := metav1.Condition{
		Type:               "Programmed",
		ObservedGeneration: httpRoute.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if affected {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "InUseByMCPServerRegistration"
		// We don't include the MCP Server in the status because >1 MCPServerRegistration may reference the same HTTPRoute
		condition.Message = "HTTPRoute is referenced by at least one MCPServerRegistration"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "NotInUse"
		condition.Message = "HTTPRoute is not referenced by any MCPServerRegistration"
	}

	found := false
	for i, cond := range httpRoute.Status.Parents {
		conditionFound := false
		for j, c := range cond.Conditions {
			if c.Type == condition.Type {
				httpRoute.Status.Parents[i].Conditions[j] = condition
				conditionFound = true
				break
			}
		}
		if !conditionFound {
			httpRoute.Status.Parents[i].Conditions = append(
				httpRoute.Status.Parents[i].Conditions,
				condition,
			)
		}
		found = true
	}

	if !found {
		log.Info("HTTPRoute has no parent statuses, skipping condition update",
			"HTTPRoute", httpRoute.Name,
			"namespace", httpRoute.Namespace)
		return nil
	}

	if err := r.Status().Update(ctx, httpRoute); err != nil {
		return fmt.Errorf("failed to update HTTPRoute status: %w", err)
	}

	log.V(1).Info("Updated HTTPRoute status",
		"HTTPRoute", httpRoute.Name,
		"namespace", httpRoute.Namespace,
		"affected", affected)

	return nil
}

func serverID(httpRoute *gatewayv1.HTTPRoute, toolPrefix, endpoint string) string {
	return fmt.Sprintf("%s:%s:%s", fmt.Sprintf("%s/%s", httpRoute.Namespace, httpRoute.Name), toolPrefix, endpoint)
}

func (r *MCPReconciler) updateStatus(
	ctx context.Context,
	mcpsr *mcpv1alpha1.MCPServerRegistration,
	ready bool,
	message string,
	toolCount int,
) error {
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "NotReady",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
	}

	statusChanged := false
	found := false
	for i, cond := range mcpsr.Status.Conditions {
		if cond.Type == condition.Type {
			// only update LastTransitionTime if the STATUS actually changed (True->False or False->True)
			// this ensures we track the time when we first entered a state, not when messages change
			if cond.Status == condition.Status {
				// status hasn't changed, preserve existing LastTransitionTime
				condition.LastTransitionTime = cond.LastTransitionTime
			}
			// check if anything actually changed
			if cond.Status != condition.Status || cond.Reason != condition.Reason || cond.Message != condition.Message {
				statusChanged = true
			}
			mcpsr.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		mcpsr.Status.Conditions = append(mcpsr.Status.Conditions, condition)
		statusChanged = true
	}
	if mcpsr.Status.DiscoveredTools != toolCount {
		mcpsr.Status.DiscoveredTools = toolCount
		statusChanged = true
	}

	// only update if something actually changed
	if !statusChanged {
		return nil
	}

	return r.Status().Update(ctx, mcpsr)
}

// SetupWithManager sets up the reconciler
func (r *MCPReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	if err := setupIndexMCPRegisrtrationToHTTPRoute(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("failed to setup required index from MCPServerRegistration to httproutes %w", err)
	}

	if err := setupIndexProgrammedHTTPRoutes(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("failed to setup required index for programmed httproutes %w", err)
	}

	controller := ctrl.NewControllerManagedBy(mgr).
		For(&mcpv1alpha1.MCPServerRegistration{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.findMCPServerRegistrationsForHTTPRoute),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findMCPServerRegistrationsForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				// only watch secrets with the credential label
				secret := obj.(*corev1.Secret)
				return secret.Labels != nil && secret.Labels[CredentialSecretLabel] == CredentialSecretValue
			})),
		).
		Watches(
			&mcpv1alpha1.MCPGatewayExtension{},
			handler.EnqueueRequestsFromMapFunc(r.findMCPServerRegistrationsForMCPGatewayExtension),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		)

	// Perform startup reconciliation to ensure config exists even with zero MCPServerRegistrations
	if err := mgr.Add(&startupReconciler{reconciler: r}); err != nil {
		return err
	}

	return controller.Complete(r)
}

func httpRouteIndexValue(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func setupIndexProgrammedHTTPRoutes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &gatewayv1.HTTPRoute{}, ProgrammedHTTPRouteIndex, func(rawObj client.Object) []string {
		httpRoute := rawObj.(*gatewayv1.HTTPRoute)
		for _, parentStatus := range httpRoute.Status.Parents {
			for _, condition := range parentStatus.Conditions {
				if condition.Type == "Programmed" && condition.Status == metav1.ConditionTrue {
					return []string{"true"}
				}
			}
		}
		return []string{"false"}
	}); err != nil {
		return err
	}
	return nil
}

func setupIndexMCPRegisrtrationToHTTPRoute(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &mcpv1alpha1.MCPServerRegistration{}, HTTPRouteIndex, func(rawObj client.Object) []string {
		mcpsr := rawObj.(*mcpv1alpha1.MCPServerRegistration)
		targetRef := mcpsr.Spec.TargetRef
		if targetRef.Kind == "HTTPRoute" {
			namespace := targetRef.Namespace
			if namespace == "" {
				namespace = mcpsr.Namespace
			}
			return []string{httpRouteIndexValue(namespace, targetRef.Name)}
		}
		return []string{}
	}); err != nil {
		return err
	}
	return nil
}

// findMCPServerRegistrationsForHTTPRoute finds all MCPServerRegistrations that reference the given HTTPRoute
func (r *MCPReconciler) findMCPServerRegistrationsForHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	httpRoute := obj.(*gatewayv1.HTTPRoute)
	log := log.FromContext(ctx).WithValues("HTTPRoute", httpRoute.Name, "namespace", httpRoute.Namespace)

	indexKey := httpRouteIndexValue(httpRoute.Namespace, httpRoute.Name)
	mcpsrList := &mcpv1alpha1.MCPServerRegistrationList{}
	if err := r.List(ctx, mcpsrList, client.MatchingFields{HTTPRouteIndex: indexKey}); err != nil {
		log.Error(err, "Failed to list MCPServerRegistrations using index")
		return nil
	}

	var requests []reconcile.Request
	for _, mcpsr := range mcpsrList.Items {
		log.V(1).Info("Found MCPServerRegistration referencing HTTPRoute via index",
			"MCPServerRegistration", mcpsr.Name,
			"MCPServerRegistrationNamespace", mcpsr.Namespace)
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      mcpsr.Name,
				Namespace: mcpsr.Namespace,
			},
		})
	}

	return requests
}

// validates credential secret has required label
func (r *MCPReconciler) validateCredentialSecret(ctx context.Context, mcpsr *mcpv1alpha1.MCPServerRegistration) error {
	if mcpsr.Spec.CredentialRef == nil {
		return nil // no credentials to validate
	}

	secret := &corev1.Secret{}
	err := r.DirectAPIReader.Get(ctx, types.NamespacedName{
		Name:      mcpsr.Spec.CredentialRef.Name,
		Namespace: mcpsr.Namespace,
	}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("credential secret %s not found", mcpsr.Spec.CredentialRef.Name)
		}
		return fmt.Errorf("failed to get credential secret: %w", err)
	}

	// check for required label
	if secret.Labels == nil || secret.Labels[CredentialSecretLabel] != CredentialSecretValue {
		return fmt.Errorf("credential secret %s is missing required label %s=%s",
			mcpsr.Spec.CredentialRef.Name, CredentialSecretLabel, CredentialSecretValue)
	}

	// validate key exists
	key := mcpsr.Spec.CredentialRef.Key
	if key == "" {
		key = "token" // default
	}
	if _, exists := secret.Data[key]; !exists {
		return fmt.Errorf("credential secret %s is missing key %s",
			mcpsr.Spec.CredentialRef.Name, key)
	}

	return nil
}

// finds mcpservers referencing the given secret
func (r *MCPReconciler) findMCPServerRegistrationsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret := obj.(*corev1.Secret)
	log := log.FromContext(ctx).WithValues("Secret", secret.Name, "namespace", secret.Namespace)

	// list mcpservers in same namespace
	mcpsrList := &mcpv1alpha1.MCPServerRegistrationList{}
	if err := r.List(ctx, mcpsrList, client.InNamespace(secret.Namespace)); err != nil {
		log.Error(err, "Failed to list MCPServerRegistrations")
		return nil
	}
	log.Info("findMCPServerRegistrationsForSecret", "total mcpserverregistrations", len(mcpsrList.Items))
	var requests []reconcile.Request
	for _, mcpsr := range mcpsrList.Items {
		// check if references this secret
		if mcpsr.Spec.CredentialRef != nil && mcpsr.Spec.CredentialRef.Name == secret.Name {
			log.Info("findMCPServerRegistrationsForSecret", "requeue", mcpsr.Name)
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      mcpsr.Name,
					Namespace: mcpsr.Namespace,
				},
			})
		}
	}

	// mcpvirtualservers don't have credentials

	return requests
}

// findMCPServerRegistrationsForMCPGatewayExtension finds all MCPServerRegistrations whose HTTPRoutes
// are attached to the Gateway targeted by the given MCPGatewayExtension. When an MCPGatewayExtension
// changes (created, updated, deleted), the associated MCPServerRegistrations need to be reconciled
// to ensure their config is written to the correct namespaces.
func (r *MCPReconciler) findMCPServerRegistrationsForMCPGatewayExtension(ctx context.Context, obj client.Object) []reconcile.Request {
	mcpExt := obj.(*mcpv1alpha1.MCPGatewayExtension)
	logger := log.FromContext(ctx).WithValues("MCPGatewayExtension", mcpExt.Name, "namespace", mcpExt.Namespace)

	// get the gateway this extension targets
	gatewayNamespace := mcpExt.Spec.TargetRef.Namespace
	if gatewayNamespace == "" {
		gatewayNamespace = mcpExt.Namespace
	}
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      mcpExt.Spec.TargetRef.Name,
		Namespace: gatewayNamespace,
	}, gateway); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to get Gateway for MCPGatewayExtension")
		}
		return nil
	}

	// find all HTTPRoutes that have this gateway as a parent
	httpRouteList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRouteList); err != nil {
		logger.Error(err, "Failed to list HTTPRoutes")
		return nil
	}

	var requests []reconcile.Request
	for _, httpRoute := range httpRouteList.Items {
		// check if this HTTPRoute references the gateway
		for _, parentRef := range httpRoute.Spec.ParentRefs {
			parentNs := httpRoute.Namespace
			if parentRef.Namespace != nil {
				parentNs = string(*parentRef.Namespace)
			}
			if string(parentRef.Name) == gateway.Name && parentNs == gateway.Namespace {
				// find MCPServerRegistrations targeting this HTTPRoute
				indexKey := httpRouteIndexValue(httpRoute.Namespace, httpRoute.Name)
				mcpsrList := &mcpv1alpha1.MCPServerRegistrationList{}
				if err := r.List(ctx, mcpsrList, client.MatchingFields{HTTPRouteIndex: indexKey}); err != nil {
					logger.Error(err, "Failed to list MCPServerRegistrations for HTTPRoute",
						"HTTPRoute", httpRoute.Name, "namespace", httpRoute.Namespace)
					continue
				}
				for _, mcpsr := range mcpsrList.Items {
					logger.V(1).Info("Enqueueing MCPServerRegistration due to MCPGatewayExtension change",
						"MCPServerRegistration", mcpsr.Name, "namespace", mcpsr.Namespace)
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      mcpsr.Name,
							Namespace: mcpsr.Namespace,
						},
					})
				}
				break
			}
		}
	}

	logger.V(1).Info("Found MCPServerRegistrations for MCPGatewayExtension", "count", len(requests))
	return requests
}

// startupReconciler ensures initial configuration is written even with zero MCPServerRegistrations
type startupReconciler struct {
	reconciler *MCPReconciler
}

// Start implements manager.Runnable
func (s *startupReconciler) Start(ctx context.Context) error {
	log := log.FromContext(ctx).WithName("startup-reconciler")
	log.Info("Running startup reconciliation to ensure config exists")

	// if err := s.reconciler.regenerateAggregatedConfig(ctx); err != nil {
	// 	log.Error(err, "Failed to run startup reconciliation")
	// 	return err
	// }

	log.Info("Startup reconciliation completed successfully")
	return nil
}
