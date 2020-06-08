package kabaneroplatform

import (
	"context"
	"fmt"
	"strings"

	kabanerov1alpha1 "github.com/kabanero-io/kabanero-operator/pkg/apis/kabanero/v1alpha1"
	kabanerov1alpha2 "github.com/kabanero-io/kabanero-operator/pkg/apis/kabanero/v1alpha2"

	ologger "github.com/kabanero-io/kabanero-operator/pkg/controller/logger"
	mf "github.com/manifestival/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var corlog = ologger.NewOperatorlogger("controller.kabaneropletform.collection-operator")

const (
	ccVersionSoftCompName   = "collection-controller"
	ccOrchestrationFileName = "collection-controller.yaml"

	ccDeploymentResourceName = "kabanero-operator-collection-controller"
)

// Installs the Kabanero collection controller.
func reconcileCollectionController(ctx context.Context, k *kabanerov1alpha2.Kabanero, c client.Client) error {
	corlog.Info(fmt.Sprintf("Reconciling Kabanero collection controller installation. Kabanero instance namespace: %v. Kabanero instance Name: %v", k.Namespace, k.Name))

	// Deploy the Kabanero collection operator.
	rev, err := resolveSoftwareRevision(k, ccVersionSoftCompName, k.Spec.CollectionController.Version)
	if err != nil {
		corlog.Error(err, "Kabanero collection controller deployment failed. Unable to resolve software revision.")
		return err
	}

	templateCtx := rev.Identifiers
	image, err := imageUriWithOverrides(k.Spec.CollectionController.Repository, k.Spec.CollectionController.Tag, k.Spec.CollectionController.Image, rev)
	if err != nil {
		corlog.Error(err, "Kabanero collection controller deployment failed. Unable to process image overrides.")
		return err
	}
	templateCtx["image"] = image
	templateCtx["instance"] = k.ObjectMeta.UID
	templateCtx["version"] = rev.Version

	f, err := rev.OpenOrchestration(ccOrchestrationFileName)
	if err != nil {
		return err
	}

	s, err := renderOrchestration(f, templateCtx)
	if err != nil {
		return err
	}

	mOrig, err := ologger.ManifestFrom(c, mf.Reader(strings.NewReader(s)), corlog)
	if err != nil {
		return err
	}

	transforms := []mf.Transformer{
		mf.InjectOwner(k),
		mf.InjectNamespace(k.GetNamespace()),
	}

	m, err := mOrig.Transform(transforms...)
	if err != nil {
		return err
	}

	err = m.Apply()
	if err != nil {
		return err
	}

	// Create a RoleBinding in the tekton-pipelines namespace that will allow
	// the collection controller to create triggerbinding and triggertemplate
	// objects in the tekton-pipelines namespace.
	templateCtx["name"] = "kabanero-" + k.GetNamespace() + "-trigger-rolebinding"
	templateCtx["kabaneroNamespace"] = k.GetNamespace()

	f, err = rev.OpenOrchestration("collection-controller-tekton.yaml")
	if err != nil {
		return err
	}

	s, err = renderOrchestration(f, templateCtx)
	if err != nil {
		return err
	}

	//mOrig, err = mf.ManifestFrom(mf.Reader(strings.NewReader(s)), mf.UseClient(mfc.NewClient(c)), mf.UseLogger(logger.WithName("manifestival")))
	mOrig, err = ologger.ManifestFrom(c, mf.Reader(strings.NewReader(s)), corlog)
	if err != nil {
		return err
	}

	err = mOrig.Apply()
	if err != nil {
		return err
	}

	return nil
}

// Removes the cross-namespace objects created during the collection controller
// deployment.
func cleanupCollectionController(ctx context.Context, k *kabanerov1alpha2.Kabanero, c client.Client) error {
	corlog.Info(fmt.Sprintf("Removing Kabanero collection controller installation. Kabanero instance namespace: %v. Kabanero instance Name: %v", k.Namespace, k.Name))

	// First, we need to delete all of the collections that we own.  We must do this first, to let the
	// collection controller run its finalizer for all of the collections, before deleting the
	// collection controller pods etc.
	collectionList := &kabanerov1alpha1.CollectionList{}
	err := c.List(ctx, collectionList, client.InNamespace(k.GetNamespace()))
	if err != nil {
		return fmt.Errorf("Unable to list collections in finalizer: %v", err.Error())
	}

	collectionCount := 0
	for _, collection := range collectionList.Items {
		for _, ownerRef := range collection.OwnerReferences {
			if ownerRef.UID == k.UID {
				collectionCount = collectionCount + 1
				if collection.DeletionTimestamp.IsZero() {
					err = c.Delete(ctx, &collection)
					if err != nil {
						// Just log the error... but continue on to the next object.
						corlog.Error(err, fmt.Sprintf("Unable to delete collection %v", collection.Name))
					}
				}
			}
		}
	}

	// If there are still some collections left, need to come back and try again later...
	if collectionCount > 0 {
		return fmt.Errorf("Deletion blocked waiting for %v owned Collections to be deleted", collectionCount)
	}

	// There used to be delete logic here for cross-namespace objects (the role binding for
	// triggers).  This is owned by the stack controler now, and so has been deleted from here.
	return nil
}

// Returns the readiness status of the Kabanero collection controller installation.
func getCollectionControllerStatus(ctx context.Context, k *kabanerov1alpha2.Kabanero, c client.Client) (bool, error) {
	k.Status.CollectionController.Message = ""
	k.Status.CollectionController.Ready = "False"

	// Retrieve the Kabanero collection controller version.
	rev, err := resolveSoftwareRevision(k, ccVersionSoftCompName, k.Spec.CollectionController.Version)
	if err != nil {
		message := "Unable to retrieve the collection controller version."
		corlog.Error(err, message)
		k.Status.CollectionController.Message = message + ": " + err.Error()
		return false, err
	}
	k.Status.CollectionController.Version = rev.Version

	// Base the status on the Kabanero collection controller's deployment resource.
	ccdeployment := &appsv1.Deployment{}
	err = c.Get(ctx, client.ObjectKey{
		Name:      ccDeploymentResourceName,
		Namespace: k.ObjectMeta.Namespace}, ccdeployment)

	if err != nil {
		message := "Unable to retrieve the Kabanero collection controller deployment object."
		corlog.Error(err, message)
		k.Status.CollectionController.Message = message + ": " + err.Error()
		return false, err
	}

	conditions := ccdeployment.Status.Conditions
	ready := false
	for _, condition := range conditions {
		if strings.ToLower(string(condition.Type)) == "available" {
			if strings.ToLower(string(condition.Status)) == "true" {
				ready = true
				k.Status.CollectionController.Ready = "True"
			} else {
				k.Status.CollectionController.Message = condition.Message
			}

			break
		}
	}

	return ready, err
}
