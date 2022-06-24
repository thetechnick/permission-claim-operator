package controllers

import (
	"fmt"

	"github.com/go-logr/logr"
	permissionsv1alpha1 "github.com/thetechnick/permission-claim-operator/apis/permissions/v1alpha1"
	"github.com/thetechnick/permission-claim-operator/internal/ownerhandling"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"
)

type PermissionClaimController struct {
	log    logr.Logger
	client client.Client
	scheme *runtime.Scheme

	baseKubeconfig *clientcmdapi.Config
	targetClient   client.Client
	targetCache    cache.Cache
	ownerStrategy  ownerStrategy
}

func NewPermissionClaimController(
	log logr.Logger,
	client client.Client,
	scheme *runtime.Scheme,
	baseKubeconfig *clientcmdapi.Config,
	targetClient client.Client,
	targetCache cache.Cache,
) *PermissionClaimController {
	return &PermissionClaimController{
		log:    log,
		client: client,
		scheme: scheme,

		baseKubeconfig: baseKubeconfig,
		targetClient:   targetClient,
		targetCache:    targetCache,
		ownerStrategy:  ownerhandling.Annotation,
	}
}

type ownerStrategy interface {
	SetControllerReference(owner, obj metav1.Object, scheme *runtime.Scheme) error
	EnqueueRequestForOwner(ownerType client.Object, isController bool) handler.EventHandler
}

func (c *PermissionClaimController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &permissionsv1alpha1.PermissionClaim{}
	if err := c.client.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.GetDeletionTimestamp().IsZero() {
		// ObjectSet was deleted.
		return ctrl.Result{}, c.handleDeletion(ctx, claim)
	}

	if err := c.ensureCacheFinalizer(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	role, err := c.reconcileRole(ctx, claim)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Role: %w", err)
	}

	clusterRole, err := c.reconcileClusterRole(ctx, claim)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ClusterRole: %w", err)
	}

	sa, err := c.reconcileServiceAccount(ctx, claim)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ServiceAccount: %w", err)
	}

	if err := c.reconcileRoleBinding(ctx, claim, role, sa); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling RoleBinding: %w", err)
	}

	if err := c.reconcileClusterRoleBinding(ctx, claim, clusterRole, sa); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ClusterRoleBinding: %w", err)
	}

	if err := c.reconcileKubeconfigSecret(ctx, claim, sa); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Kubeconfig Secret: %w", err)
	}

	return ctrl.Result{}, c.client.Status().Update(ctx, claim)
}
func (c *PermissionClaimController) reconcileKubeconfigSecret(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
	sa *corev1.ServiceAccount,
) error {
	if len(sa.Secrets) == 0 {
		// wait
		return nil
	}

	secretRef := sa.Secrets[0]
	saTokenSecret := &corev1.Secret{}
	if err := c.targetClient.Get(ctx, client.ObjectKey{
		Name:      secretRef.Name,
		Namespace: secretRef.Namespace,
	}, saTokenSecret); err != nil {
		return fmt.Errorf("getting ServiceAccount token secret: %w", err)
	}

	token := saTokenSecret.Data[corev1.ServiceAccountTokenKey]
	newKubeconfig := c.baseKubeconfig.DeepCopy()

	// replace all auth with the SA token:
	for i := range newKubeconfig.AuthInfos {
		newKubeconfig.AuthInfos[i] = &clientcmdapi.AuthInfo{
			Token: string(token),
		}
	}

	kubeconfigYaml, err := yaml.Marshal(newKubeconfig)
	if err != nil {
		panic(err)
	}

	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Spec.SecretName,
			Namespace: claim.Namespace,
		},
		Data: map[string][]byte{
			corev1.ServiceAccountKubeconfigKey: kubeconfigYaml,
		},
	}
	if err := controllerutil.SetControllerReference(claim, newSecret, c.scheme); err != nil {
		return fmt.Errorf("set controller-reference: %w", err)
	}
	if err := c.client.Create(ctx, newSecret); err != nil {
		return fmt.Errorf("creating Secret: %w", err)
	}

	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               permissionsv1alpha1.PermissionClaimBound,
		Status:             metav1.ConditionTrue,
		Reason:             "PermissionsEstablished",
		ObservedGeneration: claim.Generation,
	})
	claim.Status.Phase = permissionsv1alpha1.PermissionClaimPhaseBound
	return nil
}

func (c *PermissionClaimController) reconcileServiceAccount(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
) (*corev1.ServiceAccount, error) {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Spec.Namespace,
		},
	}
	if err := c.ownerStrategy.SetControllerReference(claim, sa, c.scheme); err != nil {
		return nil, fmt.Errorf("set controller reference: %w", err)
	}

	if err := c.client.Create(ctx, sa); err != nil {
		if errors.IsAlreadyExists(err) {
			// ok
			return sa, nil
		}
		return nil, err
	}

	return sa, nil
}

func (c *PermissionClaimController) reconcileRole(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
) (*rbacv1.Role, error) {
	desiredRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Spec.Namespace,
		},
		Rules: claim.Spec.Rules,
	}
	if err := c.ownerStrategy.SetControllerReference(claim, desiredRole, c.scheme); err != nil {
		return nil, fmt.Errorf("set controller reference: %w", err)
	}

	existingRole := &rbacv1.Role{}
	err := c.targetClient.Get(ctx, client.ObjectKeyFromObject(desiredRole), existingRole)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("getting Role: %w", err)
	}
	if errors.IsNotFound(err) {
		if err := c.targetClient.Create(ctx, desiredRole); err != nil {
			return nil, err
		}
		return desiredRole, nil
	}

	// existing Role
	if !equality.Semantic.DeepEqual(desiredRole.Rules, existingRole.Rules) {
		existingRole.Rules = desiredRole.Rules
		if err := c.targetClient.Update(ctx, existingRole); err != nil {
			return nil, fmt.Errorf("updating Role: %w", err)
		}
	}

	return desiredRole, nil
}

func (c *PermissionClaimController) reconcileClusterRole(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
) (*rbacv1.ClusterRole, error) {
	desiredRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: claim.Name,
		},
		Rules: claim.Spec.Rules,
	}
	if err := c.ownerStrategy.SetControllerReference(claim, desiredRole, c.scheme); err != nil {
		return nil, fmt.Errorf("set controller reference: %w", err)
	}

	existingRole := &rbacv1.ClusterRole{}
	err := c.targetClient.Get(ctx, client.ObjectKeyFromObject(desiredRole), existingRole)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("getting Role: %w", err)
	}
	if errors.IsNotFound(err) {
		if err := c.targetClient.Create(ctx, desiredRole); err != nil {
			return nil, err
		}
		return desiredRole, nil
	}

	// existing Role
	if !equality.Semantic.DeepEqual(desiredRole.Rules, existingRole.Rules) {
		existingRole.Rules = desiredRole.Rules
		if err := c.targetClient.Update(ctx, existingRole); err != nil {
			return nil, fmt.Errorf("updating Role: %w", err)
		}
	}

	return desiredRole, nil
}

func (c *PermissionClaimController) reconcileRoleBinding(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
	role *rbacv1.Role, sa *corev1.ServiceAccount,
) error {
	roleGVK, _ := apiutil.GVKForObject(role.DeepCopy(), c.scheme)
	saGVK, _ := apiutil.GVKForObject(sa.DeepCopyObject(), c.scheme)
	desiredRole := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Spec.Namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: roleGVK.Group,
			Kind:     roleGVK.Kind,
			Name:     role.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: saGVK.Group,
				Kind:     saGVK.Kind,
				Name:     sa.Name,
			},
		},
	}
	if err := c.ownerStrategy.SetControllerReference(claim, desiredRole, c.scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}

	if err := c.client.Create(ctx, desiredRole); err != nil {
		if errors.IsAlreadyExists(err) {
			// ok
			return nil
		}
		return err
	}

	return nil
}

func (c *PermissionClaimController) reconcileClusterRoleBinding(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
	role *rbacv1.ClusterRole, sa *corev1.ServiceAccount,
) error {
	roleGVK, _ := apiutil.GVKForObject(role.DeepCopy(), c.scheme)
	saGVK, _ := apiutil.GVKForObject(sa.DeepCopyObject(), c.scheme)
	desiredRole := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claim.Name,
			Namespace: claim.Spec.Namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: roleGVK.Group,
			Kind:     roleGVK.Kind,
			Name:     role.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup:  saGVK.Group,
				Kind:      saGVK.Kind,
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		},
	}
	if err := c.ownerStrategy.SetControllerReference(claim, desiredRole, c.scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}

	if err := c.client.Create(ctx, desiredRole); err != nil {
		if errors.IsAlreadyExists(err) {
			// ok
			return nil
		}
		return err
	}

	return nil
}

func (c *PermissionClaimController) SetupWithManager(mgr ctrl.Manager) error {
	t := &permissionsv1alpha1.PermissionClaim{}
	h := c.ownerStrategy.EnqueueRequestForOwner(t, true)

	return ctrl.NewControllerManagedBy(mgr).
		For(t).
		Owns(&corev1.Secret{}).
		Watches(
			source.NewKindWithCache(&corev1.ServiceAccount{}, c.targetCache),
			h,
		).
		Watches(
			source.NewKindWithCache(&rbacv1.ClusterRole{}, c.targetCache),
			h,
		).
		Watches(
			source.NewKindWithCache(&rbacv1.Role{}, c.targetCache),
			h,
		).
		Watches(
			source.NewKindWithCache(&rbacv1.ClusterRoleBinding{}, c.targetCache),
			h,
		).
		Watches(
			source.NewKindWithCache(&rbacv1.RoleBinding{}, c.targetCache),
			h,
		).
		Complete(c)
}

const cleanupFinalizer = "permissions.thetechnick.ninja/cleanup"

func (c *PermissionClaimController) handleDeletion(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
) error {
	// Delete stuff
	objs := []client.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}},
	}
	for _, obj := range objs {
		if err := c.targetClient.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("cleanup on target cluster: %w", err)
		}
	}

	if controllerutil.ContainsFinalizer(claim, cleanupFinalizer) {
		controllerutil.RemoveFinalizer(claim, cleanupFinalizer)

		if err := c.client.Update(ctx, claim); err != nil {
			return fmt.Errorf("removing finalizer: %w", err)
		}
	}
	return nil
}

// ensures the cache finalizer is set on the given object
func (c *PermissionClaimController) ensureCacheFinalizer(
	ctx context.Context, claim *permissionsv1alpha1.PermissionClaim,
) error {
	if !controllerutil.ContainsFinalizer(claim, cleanupFinalizer) {
		controllerutil.AddFinalizer(claim, cleanupFinalizer)
		if err := c.client.Update(ctx, claim); err != nil {
			return fmt.Errorf("adding finalizer: %w", err)
		}
	}
	return nil
}
