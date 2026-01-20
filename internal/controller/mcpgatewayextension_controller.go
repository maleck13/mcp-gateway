package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcpv1alpha1 "github.com/Kuadrant/mcp-gateway/api/v1alpha1"
	"github.com/Kuadrant/mcp-gateway/internal/config"
	"github.com/go-logr/logr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	mcpGatewayFinalizer = "mcp.kagenti.com/finalizer"
	// gatewayIndexKey is the index used to improve look up of mcpgatewayextensions related to a gateway
	gatewayIndexKey  = "spec.targetRef.gateway"
	refGrantIndexKey = "spec.from.ref"
)

type ConfigWriterDeleter interface {
	DeleteConfig(ctx context.Context, namespaceName types.NamespacedName) error
	EnsureConfigExists(ctx context.Context, namespaceName types.NamespacedName) error
}

// MCPGatewayExtensionReconciler reconciles a MCPGatewayExtension object
type MCPGatewayExtensionReconciler struct {
	client.Client
	DirectAPIReader     client.Reader
	Scheme              *runtime.Scheme
	log                 logr.Logger
	ConfigWriterDeleter ConfigWriterDeleter
}

// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpgatewayextensions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpgatewayextensions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcp.kagenti.com,resources=mcpgatewayextensions/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MCPGatewayExtension object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *MCPGatewayExtensionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	mcpExt := &mcpv1alpha1.MCPGatewayExtension{}
	if err := r.Get(ctx, req.NamespacedName, mcpExt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("reconciling mcpgatewayextension", "name", mcpExt.Name)

	// handle deletion
	if !mcpExt.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(mcpExt, mcpGatewayFinalizer) {
			logger.Info("deleting mcpgatewayextension", "name", mcpExt.Name, "namespace", mcpExt.Namespace)
			// TODO delete config from this namespace
			if err := r.ConfigWriterDeleter.DeleteConfig(ctx, config.ConfigNamespaceName(mcpExt.Namespace)); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(mcpExt, mcpGatewayFinalizer)
			if err := r.Update(ctx, mcpExt); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// add finalizer if not present
	if !controllerutil.ContainsFinalizer(mcpExt, mcpGatewayFinalizer) {
		controllerutil.AddFinalizer(mcpExt, mcpGatewayFinalizer)
		if err := r.Update(ctx, mcpExt); err != nil {
			return ctrl.Result{}, err
		}
	}
	// main reconcile status logic
	// TODO ensure there is actually a mcpgateway instance in the mcpgatewayextension namespace
	// check which gateway this resource is targeting and if in same namespace
	inSameNS := mcpExt.Spec.TargetRef.Namespace == mcpExt.Namespace
	targetGateway, err := r.gatewayTarget(ctx, mcpExt.Spec.TargetRef)
	if err != nil {
		if errors.IsNotFound(err) {
			err := r.setReadyConditionAndUpdateStatus(ctx, metav1.ConditionFalse, mcpv1alpha1.ConditionReasonInvalid, "invalid", mcpExt)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	// not in same namespace - check for a valid ReferenceGrant
	if !inSameNS {
		hasGrant, err := r.hasValidReferenceGrant(ctx, mcpExt)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !hasGrant {
			logger.Info("no valid ReferenceGrant found for cross-namespace reference. Removing config and setting status",
				"mcpgatewayextension", mcpExt.Name,
				"mcpgatewayextension-namespace", mcpExt.Namespace,
				"gateway-namespace", mcpExt.Spec.TargetRef.Namespace)
			if err := r.ConfigWriterDeleter.DeleteConfig(ctx, config.ConfigNamespaceName(mcpExt.Namespace)); err != nil {
				return ctrl.Result{}, err
			}
			err := r.setReadyConditionAndUpdateStatus(ctx, metav1.ConditionFalse, mcpv1alpha1.ConditionReasonRefGrantRequired,
				fmt.Sprintf("ReferenceGrant required in namespace %s to allow cross-namespace reference", mcpExt.Spec.TargetRef.Namespace), mcpExt)
			return ctrl.Result{}, err
		}
	}

	// check if this gateway is already targeted by another MCPGatewayExtension
	existingExts, err := r.listMCPGatewayExtsForGateway(ctx, targetGateway)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(existingExts.Items) > 1 {
		owner := mcpExt
		for i := range existingExts.Items {
			if owner.CreationTimestamp.After(existingExts.Items[i].CreationTimestamp.Time) {
				owner = &existingExts.Items[i]
			}
		}
		if owner.GetUID() != mcpExt.GetUID() {
			if err := r.setReadyConditionAndUpdateStatus(ctx, metav1.ConditionFalse, mcpv1alpha1.ConditionReasonInvalid, "conflict. Gateway target already configured as MCP Gateway", mcpExt); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}
	// valid configuration
	err = r.setReadyConditionAndUpdateStatus(ctx, metav1.ConditionTrue, mcpv1alpha1.ConditionReasonSuccess, "successfully verified and configured", mcpExt)
	if err != nil {
		return ctrl.Result{}, err
	}
	// create an empty config if a config doesn't exist
	if err := r.ConfigWriterDeleter.EnsureConfigExists(ctx, config.ConfigNamespaceName(mcpExt.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, err
}

func (r *MCPGatewayExtensionReconciler) setReadyConditionAndUpdateStatus(ctx context.Context, conditionStatus metav1.ConditionStatus, conditionReason, message string, mcpExt *mcpv1alpha1.MCPGatewayExtension) error {
	// copy existing condition before modifying (FindStatusCondition returns a pointer that gets modified in place)
	var existingCondition *metav1.Condition
	if existing := meta.FindStatusCondition(mcpExt.Status.Conditions, mcpv1alpha1.ConditionTypeReady); existing != nil {
		copied := *existing
		existingCondition = &copied
	}
	mcpExt.SetReadyCondition(conditionStatus, conditionReason, message)
	updatedCondition := meta.FindStatusCondition(mcpExt.Status.Conditions, mcpv1alpha1.ConditionTypeReady)
	if !equality.Semantic.DeepEqual(existingCondition, updatedCondition) {
		if err := r.Status().Update(ctx, mcpExt); err != nil {
			return err
		}
	}
	return nil
}

func (r *MCPGatewayExtensionReconciler) gatewayTarget(ctx context.Context, target mcpv1alpha1.MCPGatewayExtensionTargetReference) (*gatewayv1.Gateway, error) {
	g := &gatewayv1.Gateway{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, g); err != nil {
		// return NotFound errors unwrapped so caller can check with errors.IsNotFound
		if errors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to find gateway targeted by the mcpgatewayextension %w", err)
	}
	return g, nil
}

// hasValidReferenceGrant checks if a valid ReferenceGrant exists that allows the MCPGatewayExtension
// to reference a Gateway in a different namespace
func (r *MCPGatewayExtensionReconciler) hasValidReferenceGrant(ctx context.Context, mcpExt *mcpv1alpha1.MCPGatewayExtension) (bool, error) {
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
func (r *MCPGatewayExtensionReconciler) referenceGrantAllows(rg *gatewayv1beta1.ReferenceGrant, mcpExt *mcpv1alpha1.MCPGatewayExtension) bool {
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

func mcpExtToRefGrantIndexValue(mext mcpv1alpha1.MCPGatewayExtension) string {
	//TODO check if this index is really needed?
	return fmt.Sprintf("%s%s%s", mext.GroupVersionKind().Group, mext.GroupVersionKind().Kind, mext.Namespace)

}

func mcpExtToGatewayIndexValue(mext mcpv1alpha1.MCPGatewayExtension) string {
	return fmt.Sprintf("%s%s", mext.Spec.TargetRef.Name, mext.Spec.TargetRef.Namespace)
}

func gatewayToMCPExtIndexValue(g gatewayv1.Gateway) string {
	return fmt.Sprintf("%s%s", g.Name, g.Namespace)
}

func refGrantToMCPExtIndexValue(r gatewayv1beta1.ReferenceGrant) string {
	// the spec.from is a required field so will be set
	f := r.Spec.From[0]
	return fmt.Sprintf("%s%s%s", f.Group, f.Kind, f.Namespace)
}

// setupIndexExtensionToGateway creates an index for the gateway targeted by an MCPGatewayExtension
// This improves discovery when the controller receives a gateway change event
func setupIndexExtensionToGateway(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(
		ctx,
		&mcpv1alpha1.MCPGatewayExtension{},
		gatewayIndexKey,
		func(obj client.Object) []string {
			mcpExt, ok := obj.(*mcpv1alpha1.MCPGatewayExtension)
			if ok {
				return []string{mcpExtToGatewayIndexValue(*mcpExt)}
			}
			return []string{}
		},
	); err != nil {
		return fmt.Errorf("failed to setup index mcpgatewayextension to gateway %w", err)
	}
	return nil
}

// setupIndexExtensionToGateway creates an index for the ReferenceGrant allowing an MCPGatewayExtension to target a gateway
// This improves discovery when the controller receives a ReferenceGrant change event
func setupIndexExtensionToReferenceGrant(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(
		ctx,
		&mcpv1alpha1.MCPGatewayExtension{},
		refGrantIndexKey,
		func(obj client.Object) []string {
			mcpGatewayExt, ok := obj.(*mcpv1alpha1.MCPGatewayExtension)
			if ok {
				return []string{mcpExtToRefGrantIndexValue(*mcpGatewayExt)}
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to setup index mcpgatewayextension to reference grants %w", err)
	}
	return nil
}

func (r *MCPGatewayExtensionReconciler) listMCPGatewayExtsForGateway(ctx context.Context, g *gatewayv1.Gateway) (*mcpv1alpha1.MCPGatewayExtensionList, error) {
	mcpGatewayExtList := &mcpv1alpha1.MCPGatewayExtensionList{}
	if err := r.List(ctx, mcpGatewayExtList,
		client.MatchingFields{gatewayIndexKey: gatewayToMCPExtIndexValue(*g)},
	); err != nil {
		return mcpGatewayExtList, err
	}
	return mcpGatewayExtList, nil
}

func (r *MCPGatewayExtensionReconciler) findMCPGatewayExtForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	requests := []reconcile.Request{}
	if !ok {
		return requests
	}
	mcpGatewayExtList, err := r.listMCPGatewayExtsForGateway(ctx, gateway)
	if err != nil {
		// just log as this is adhering to the EnqueueRequestsFromMapFunc signature
		r.log.Error(err, "failed to list existing mcpgatewayextension for gateway ", "gateway", gateway)
		return requests
	}
	//r.log.V(1).Info("found mcpgatewayextensions by gateway ", "total", len(mcpGatewayExtList.Items), "gateway", gateway.Name)
	for _, ext := range mcpGatewayExtList.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ext)})
	}
	return requests
}

func (r *MCPGatewayExtensionReconciler) findMCPGatewayExtForReferenceGrant(ctx context.Context, obj client.Object) []reconcile.Request {
	ref, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	requests := []reconcile.Request{}
	if !ok {
		return requests
	}
	if len(ref.Spec.From) == 0 {
		return requests
	}
	mcpGatewayExtList := &mcpv1alpha1.MCPGatewayExtensionList{}
	if err := r.List(ctx, mcpGatewayExtList,
		client.MatchingFields{refGrantIndexKey: refGrantToMCPExtIndexValue(*ref)},
	); err != nil {
		r.log.Error(err, "failed to findMCPGatewayExtForGateway")
		return nil
	}
	r.log.V(1).Info("found mcpgatewayextensions by refgrant ", "total", len(mcpGatewayExtList.Items), "referencegrant", ref.Name)
	for _, ext := range mcpGatewayExtList.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ext)})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *MCPGatewayExtensionReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.log = mgr.GetLogger()
	if err := setupIndexExtensionToGateway(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("failed to setup manager %w", err)
	}

	if err := setupIndexExtensionToReferenceGrant(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("failed to setup manager %w", err)
	}

	// enqueue mcpgateway extensions when the gateway changes
	// enqueue when reference grants change
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcpv1alpha1.MCPGatewayExtension{}).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.findMCPGatewayExtForGateway)).
		Watches(&gatewayv1beta1.ReferenceGrant{}, handler.EnqueueRequestsFromMapFunc(r.findMCPGatewayExtForReferenceGrant)).
		Named("mcpgatewayextension").
		Complete(r)
}
