package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-openapi/inflect"
	"github.com/libopenstorage/stork/drivers/volume"
	stork_api "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	storkcache "github.com/libopenstorage/stork/pkg/cache"
	"github.com/libopenstorage/stork/pkg/controllers"
	"github.com/libopenstorage/stork/pkg/k8sutils"
	"github.com/libopenstorage/stork/pkg/log"
	"github.com/libopenstorage/stork/pkg/resourcecollector"
	"github.com/libopenstorage/stork/pkg/rule"
	"github.com/libopenstorage/stork/pkg/utils"
	"github.com/libopenstorage/stork/pkg/version"
	"github.com/mitchellh/hashstructure"
	"github.com/portworx/sched-ops/k8s/apiextensions"
	"github.com/portworx/sched-ops/k8s/core"
	storkops "github.com/portworx/sched-ops/k8s/stork"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/slice"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/registry/core/service/portallocator"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// StorkMigrationReplicasAnnotation is the annotation used to keep track of
	// the number of replicas for an application when it was migrated
	StorkMigrationReplicasAnnotation = "stork.libopenstorage.org/migrationReplicas"
	// StorkMigrationAnnotation is the annotation used to keep track of resources
	// migrated by stork
	StorkMigrationAnnotation = "stork.libopenstorage.org/migrated"
	// StorkMigrationName is the annotation used to identify resource migrated by
	// migration CRD name
	StorkMigrationName = "stork.libopenstorage.org/migrationName"
	// StorkMigrationNamespace is the annotation used to identify migration CRD's namespace
	StorkMigrationNamespace = "stork.libopenstorage.org/migrationNamespace"
	// StorkMigrationTime is the annotation used to specify time of migration
	StorkMigrationTime = "stork.libopenstorage.org/migrationTime"
	// StorkMigrationCRDActivateAnnotation is the annotation used to keep track of
	// the value to be set for activating crds
	StorkMigrationCRDActivateAnnotation = "stork.libopenstorage.org/migrationCRDActivate"
	// StorkMigrationCRDDeactivateAnnotation is the annotation used to keep track of
	// the value to be set for deactivating crds
	StorkMigrationCRDDeactivateAnnotation = "stork.libopenstorage.org/migrationCRDDeactivate"
	// PVReclaimAnnotation for pvc's reclaim policy
	PVReclaimAnnotation = "stork.libopenstorage.org/reclaimPolicy"
	// StorkAnnotationPrefix for resources created/managed by stork
	StorkAnnotationPrefix = "stork.libopenstorage.org/"
	// StorkNamespacePrefix for namespace created for applying dry run resources
	StorkNamespacePrefix = "stork-transform"
	// StashCRLabel is the label used for stashed configmaps for the CRs if stash strategy is enabled
	StashCRLabel         = "stash-cr"
	StashedCMOwnedPVCKey = "ownedPVCs"
	StashedCMCRKey       = "cr-runtime-object"
	StashedCMCRNameKey   = "name"
	// Max number of times to retry applying resources on the destination
	maxApplyRetries      = 10
	deletedMaxRetries    = 12
	deletedRetryInterval = 10 * time.Second
	boundRetryInterval   = 5 * time.Second
	applyRetryInterval   = 5 * time.Second
)

var (
	AutoCreatedPrefixes    = []string{"builder-dockercfg-", "builder-token-", "default-dockercfg-", "default-token-", "deployer-dockercfg-", "deployer-token-"}
	catergoriesExcludeList = []string{"all", "olm", "coreoperators"}

	ErrReapplyLatestVersionMsg = "please apply your changes to the latest version and try again"
)

// NewMigration creates a new instance of MigrationController.
func NewMigration(mgr manager.Manager, d volume.Driver, r record.EventRecorder, rc resourcecollector.ResourceCollector) *MigrationController {
	return &MigrationController{
		client:            mgr.GetClient(),
		volDriver:         d,
		recorder:          r,
		resourceCollector: rc,
	}
}

// MigrationController reconciles migration objects
type MigrationController struct {
	client runtimeclient.Client

	volDriver               volume.Driver
	recorder                record.EventRecorder
	resourceCollector       resourcecollector.ResourceCollector
	migrationAdminNamespace string
	migrationMaxThreads     int
}

// RemoteConfig contains config and clients to interact with destination cluster
type RemoteClient struct {
	remoteConfig         *rest.Config
	remoteAdminConfig    *rest.Config
	adminClient          *kubernetes.Clientset
	remoteInterface      dynamic.Interface
	remoteAdminInterface dynamic.Interface
}

// Init Initialize the migration controller
func (m *MigrationController) Init(mgr manager.Manager, migrationAdminNamespace string, migrationMaxThreads int) error {
	err := m.createCRD()
	if err != nil {
		return err
	}

	m.migrationMaxThreads = migrationMaxThreads
	m.migrationAdminNamespace = migrationAdminNamespace
	if err := m.performRuleRecovery(); err != nil {
		logrus.Errorf("Failed to perform recovery for migration rules: %v", err)
		return err
	}

	return controllers.RegisterTo(mgr, "migration-controller", m, &stork_api.Migration{})
}

// Reconcile manages Migration resources.
func (m *MigrationController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	logrus.Tracef("Reconciling Migration %s/%s", request.Namespace, request.Name)

	// Fetch the Migration instance
	migration := &stork_api.Migration{}
	err := m.client.Get(context.TODO(), request.NamespacedName, migration)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	if !controllers.ContainsFinalizer(migration, controllers.FinalizerCleanup) {
		controllers.SetFinalizer(migration, controllers.FinalizerCleanup)
		return reconcile.Result{Requeue: true}, m.client.Update(context.TODO(), migration)
	}

	if err = m.handle(context.TODO(), migration); err != nil {
		logrus.Errorf("%s: %s/%s: %s", reflect.TypeOf(m), migration.Namespace, migration.Name, err)
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	return reconcile.Result{RequeueAfter: controllers.DefaultRequeue}, nil
}

func setKind(snap *stork_api.Migration) {
	snap.Kind = "Migration"
	snap.APIVersion = stork_api.SchemeGroupVersion.String()
}

// performRuleRecovery terminates potential background commands running pods for
// all migration objects
func (m *MigrationController) performRuleRecovery() error {
	migrations, err := storkops.Instance().ListMigrations(v1.NamespaceAll)
	if err != nil {
		logrus.Errorf("Failed to list all migrations during rule recovery: %v", err)
		return err
	}

	if migrations == nil {
		return nil
	}

	var lastError error
	for _, migration := range migrations.Items {
		setKind(&migration)
		err := rule.PerformRuleRecovery(&migration)
		if err != nil {
			lastError = err
		}
	}
	return lastError
}

func setDefaults(spec stork_api.MigrationSpec) stork_api.MigrationSpec {
	if spec.IncludeVolumes == nil {
		defaultBool := true
		spec.IncludeVolumes = &defaultBool
	}
	if spec.IncludeResources == nil {
		defaultBool := true
		spec.IncludeResources = &defaultBool
	}
	if spec.StartApplications == nil {
		defaultBool := false
		spec.StartApplications = &defaultBool
	}
	if spec.PurgeDeletedResources == nil {
		defaultBool := false
		spec.PurgeDeletedResources = &defaultBool
	}
	if spec.SkipServiceUpdate == nil {
		defaultBool := false
		spec.SkipServiceUpdate = &defaultBool
	}
	if spec.IncludeNetworkPolicyWithCIDR == nil {
		defaultBool := false
		spec.IncludeNetworkPolicyWithCIDR = &defaultBool
	}
	if spec.SkipDeletedNamespaces == nil {
		defaultBool := true
		spec.SkipDeletedNamespaces = &defaultBool
	}
	if spec.IgnoreOwnerReferencesCheck == nil {
		defaultBool := false
		spec.IgnoreOwnerReferencesCheck = &defaultBool
	}
	return spec
}

func (m *MigrationController) updateMigrationCR(ctx context.Context, migration *stork_api.Migration) error {
	migration.Status.Summary = m.getMigrationSummary(migration)
	return m.client.Update(ctx, migration)
}

func (m *MigrationController) handle(ctx context.Context, migration *stork_api.Migration) error {
	if migration.DeletionTimestamp != nil {
		if controllers.ContainsFinalizer(migration, controllers.FinalizerCleanup) {
			if err := m.cleanup(migration); err != nil {
				logrus.Errorf("%s: cleanup: %s", reflect.TypeOf(m), err)
			}
		}

		if migration.GetFinalizers() != nil {
			controllers.RemoveFinalizer(migration, controllers.FinalizerCleanup)
			return m.client.Update(ctx, migration)
		}

		return nil
	}

	migration.Spec = setDefaults(migration.Spec)

	if migration.GetAnnotations() != nil {
		if schedName, ok := migration.GetAnnotations()[StorkMigrationScheduleName]; ok {
			remoteConfig, err := getClusterPairSchedulerConfig(migration.Spec.ClusterPair, migration.Namespace)
			if err != nil {
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					err.Error())
				return err
			}
			remoteOps, err := storkops.NewForConfig(remoteConfig)
			if err != nil {
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					err.Error())
				return nil
			}
			autoSuspend := true
			// get remote cluster migration schedule object
			remoteMigrSched, err := remoteOps.GetMigrationSchedule(schedName, migration.Namespace)
			if err != nil {
				if errors.IsNotFound(err) {
					// migrSchedule object not present on remote,
					// that means autosuspend feature is disabled
					autoSuspend = false
				} else {
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						err.Error())
					return nil
				}
			}
			// check status of migrated app on remote cluster
			// if apps are online mark status of current migration as failed
			// if its in progress state
			if autoSuspend && remoteMigrSched.Status.ApplicationActivated && migration.Status.Stage != stork_api.MigrationStageFinal {
				migration.Status.Status = stork_api.MigrationStatusFailed
				migration.Status.Stage = stork_api.MigrationStageFinal
				migration.Status.FinishTimestamp = metav1.Now()
				err = fmt.Errorf("migrated applications are active on remote cluster")
				log.MigrationLog(migration).Errorf(err.Error())
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					err.Error())
				err = m.client.Update(context.Background(), migration)
				if err != nil {
					log.MigrationLog(migration).Errorf("Error updating")
				}
				return nil
			}
		}
	}

	if migration.Spec.ClusterPair == "" {
		err := fmt.Errorf("clusterPair to migrate to cannot be empty")
		log.MigrationLog(migration).Errorf(err.Error())
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			err.Error())
		return nil
	}
	migrationNamespaces, err := m.getMigrationNamespaces(ctx, migration)
	if err != nil {
		err := fmt.Errorf("unable to extract migration namespaces")
		log.MigrationLog(migration).Errorf(err.Error())
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			err.Error())
		return nil
	}
	if len(migrationNamespaces) == 0 {
		err := fmt.Errorf("no valid namespace found based on the provided 'Namespaces' and 'NamespaceSelectors'")
		log.MigrationLog(migration).Errorf(err.Error())
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			err.Error())
		return nil
	}
	// Check whether namespace is allowed to be migrated before each stage
	// Restrict migration to only the namespace that the object belongs
	// except for the namespace designated by the admin
	if !m.namespaceMigrationAllowed(migration, migrationNamespaces) {
		err := fmt.Errorf("migration namespaces should only contain the current namespace")
		log.MigrationLog(migration).Errorf(err.Error())
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			err.Error())
		return nil
	}
	var terminationChannels []chan bool
	var clusterDomains *stork_api.ClusterDomains
	if !*migration.Spec.IncludeVolumes {
		for i := 0; i < domainsMaxRetries; i++ {
			clusterDomains, err = m.volDriver.GetClusterDomains()
			if err == nil {

				break
			}
			time.Sleep(domainsRetryInterval)
		}
		// Fail the migration if the current domain is inactive
		// Ignore errors
		if err == nil {
			for _, domainInfo := range clusterDomains.ClusterDomainInfos {
				if domainInfo.Name == clusterDomains.LocalDomain &&
					domainInfo.State == stork_api.ClusterDomainInactive &&
					migration.Status.Stage != stork_api.MigrationStageFinal {
					migration.Status.Status = stork_api.MigrationStatusFailed
					migration.Status.Stage = stork_api.MigrationStageFinal
					migration.Status.FinishTimestamp = metav1.Now()
					msg := "Failing migration since local clusterdomain is inactive"
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						msg)
					log.MigrationLog(migration).Warn(msg)
					return m.updateMigrationCR(context.TODO(), migration)
				}
			}
		}
	}

	switch migration.Status.Stage {
	case stork_api.MigrationStageInitial:
		// Make sure the namespaces exist
		for _, ns := range migrationNamespaces {
			_, err := core.Instance().GetNamespace(ns)
			if err != nil {
				if migration.Spec.SkipDeletedNamespaces != nil && *migration.Spec.SkipDeletedNamespaces {
					// Instead of throwing an error here check for the SkipDeletedNamespaces  flag
					// and based on that either throw an error or continue for deleted namespaces
					migration.Status.Status = stork_api.MigrationStatusInitial
					migration.Status.Stage = stork_api.MigrationStageInitial
					migration.Status.FinishTimestamp = metav1.Now()
					skipWarning := fmt.Sprintf("namespace  %s was deleted, skipping migration", ns)
					log.MigrationLog(migration).Warnf(skipWarning)
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						skipWarning,
						err.Error())
				} else {
					migration.Status.Status = stork_api.MigrationStatusFailed
					migration.Status.Stage = stork_api.MigrationStageFinal
					migration.Status.FinishTimestamp = metav1.Now()
					err = fmt.Errorf("error getting namespace %v: %v", ns, err)
					log.MigrationLog(migration).Errorf(err.Error())
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						err.Error())
					err = m.updateMigrationCR(context.Background(), migration)
					if err != nil {
						log.MigrationLog(migration).Errorf("Error updating CR, err: %v", err)
					}
					return nil
				}
			}
			// Make sure if transformation CR is in ready state
			if len(migration.Spec.TransformSpecs) != 0 {
				// Check if multiple transformation specs are provided
				if len(migration.Spec.TransformSpecs) > 1 {
					errMsg := fmt.Sprintf("providing multiple transformation specs is not supported in this release %v, err: %v", migration.Spec.TransformSpecs, err)
					log.MigrationLog(migration).Errorf(errMsg)
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						errMsg)
					err = m.updateMigrationCR(context.Background(), migration)
					if err != nil {
						log.MigrationLog(migration).Errorf("Error updating CR, err: %v", err)
					}
					return nil
				}
				// verify if transform specs are created
				resp, err := storkops.Instance().GetResourceTransformation(migration.Spec.TransformSpecs[0], ns)
				if err != nil {
					errMsg := fmt.Sprintf("unable to retrieve transformation %s, err: %v", migration.Spec.TransformSpecs, err)
					log.MigrationLog(migration).Errorf(errMsg)
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						err.Error())
					err = m.updateMigrationCR(context.Background(), migration)
					if err != nil {
						log.MigrationLog(migration).Errorf("Error updating CR, err: %v", err)
					}
					return nil
				}
				if err := storkops.Instance().ValidateResourceTransformation(resp.Name, ns, 1*time.Minute, 5*time.Second); err != nil {
					errMsg := fmt.Sprintf("transformation %s is not in ready state: %s", migration.Spec.TransformSpecs[0], resp.Status.Status)
					log.MigrationLog(migration).Errorf(errMsg)
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						errMsg)
					err = m.updateMigrationCR(context.Background(), migration)
					if err != nil {
						log.MigrationLog(migration).Errorf("Error updating CR, err: %v", err)
					}
					return nil
				}
			}
		}
		// Make sure the rules exist if configured
		if migration.Spec.PreExecRule != "" {
			_, err := storkops.Instance().GetRule(migration.Spec.PreExecRule, migration.Namespace)
			if err != nil {
				message := fmt.Sprintf("Error getting PreExecRule %v: %v", migration.Spec.PreExecRule, err)
				log.MigrationLog(migration).Errorf(message)
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					message)
				return nil
			}
		}
		if migration.Spec.PostExecRule != "" {
			_, err := storkops.Instance().GetRule(migration.Spec.PostExecRule, migration.Namespace)
			if err != nil {
				message := fmt.Sprintf("Error getting PostExecRule %v: %v", migration.Spec.PreExecRule, err)
				log.MigrationLog(migration).Errorf(message)
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					message)
				return nil
			}
		}
		fallthrough
	case stork_api.MigrationStagePreExecRule:
		terminationChannels, err = m.runPreExecRule(migration, migrationNamespaces)
		if err != nil {
			message := fmt.Sprintf("Error running PreExecRule: %v", err)
			log.MigrationLog(migration).Errorf(message)
			m.recorder.Event(migration,
				v1.EventTypeWarning,
				string(stork_api.MigrationStatusFailed),
				message)
			migration.Status.Stage = stork_api.MigrationStageInitial
			migration.Status.Status = stork_api.MigrationStatusInitial
			err := m.updateMigrationCR(context.Background(), migration)
			if err != nil {
				return err
			}
			return nil
		}
		fallthrough
	case stork_api.MigrationStageVolumes:
		if *migration.Spec.IncludeVolumes {
			err := m.migrateVolumes(migration, migrationNamespaces, terminationChannels)
			if err != nil {
				message := fmt.Sprintf("Error migrating volumes: %v", err)
				log.MigrationLog(migration).Errorf(message)
				// Don't need to log this event as Stork retries if it fails to update
				if !strings.Contains(message, ErrReapplyLatestVersionMsg) {
					m.recorder.Event(migration,
						v1.EventTypeWarning,
						string(stork_api.MigrationStatusFailed),
						message)
					return nil
				}
			}
		} else {
			migration.Status.Stage = stork_api.MigrationStageApplications
			migration.Status.Status = stork_api.MigrationStatusInitial
			migration.Status.VolumeMigrationFinishTimestamp = metav1.Now()
			err := m.updateMigrationCR(context.Background(), migration)
			if err != nil {
				return err
			}
		}
	case stork_api.MigrationStageApplications:
		var volumesOnly bool
		if migration.Spec.IncludeResources != nil && !*migration.Spec.IncludeResources {
			// Include Resources is set to false
			// This is a volumeOnly migration
			volumesOnly = true
		}
		err := m.migrateResources(migration, migrationNamespaces, volumesOnly)
		if err != nil {
			message := fmt.Sprintf("Error migrating resources: %v", err)
			log.MigrationLog(migration).Errorf(message)
			// Don't need to log this event as Stork retries if it fails to update
			if !strings.Contains(message, ErrReapplyLatestVersionMsg) {
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					message)
				return nil
			}
		}
	case stork_api.MigrationStageFinal:
		return nil
	default:
		log.MigrationLog(migration).Errorf("Invalid stage for migration: %v", migration.Status.Stage)
	}

	return nil
}

func (m *MigrationController) getMigrationNamespaces(ctx context.Context, migration *stork_api.Migration) ([]string, error) {
	var migrationNamespaces []string
	uniqueNamespaces := make(map[string]bool)

	for _, ns := range migration.Spec.Namespaces {
		uniqueNamespaces[ns] = true
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	for key, val := range migration.Spec.NamespaceSelectors {
		label := key + "=" + val
		namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return nil, err
		}
		for _, namespace := range namespaces.Items {
			uniqueNamespaces[namespace.GetName()] = true
		}
	}

	for namespace := range uniqueNamespaces {
		migrationNamespaces = append(migrationNamespaces, namespace)
	}
	return migrationNamespaces, nil
}

func (m *MigrationController) purgeMigratedResources(
	migration *stork_api.Migration,
	migrationNamespaces []string,
	resourceCollectorOpts resourcecollector.Options,
) error {
	remoteConfig, err := getClusterPairSchedulerConfig(migration.Spec.ClusterPair, migration.Namespace)
	if err != nil {
		return err
	}

	log.MigrationLog(migration).Infof("Purging old unused resources ...")
	// use seperate resource collector for collecting resources
	// from destination cluster
	rc := resourcecollector.ResourceCollector{
		Driver: m.volDriver,
	}
	err = rc.Init(remoteConfig)
	if err != nil {
		log.MigrationLog(migration).Errorf("Error initializing resource collector: %v", err)
		return err
	}
	excludeSelectors := make(map[string]string)
	if migration.Spec.ExcludeSelectors != nil {
		excludeSelectors = migration.Spec.ExcludeSelectors
	}
	excludeSelectors[StashCRLabel] = "true"

	destObjects, _, err := m.getResources(
		migrationNamespaces,
		migration,
		migration.Spec.Selectors,
		excludeSelectors,
		resourceCollectorOpts,
		true,
	)

	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error getting resources from destination: %v", err))
		log.MigrationLog(migration).Errorf("Error getting resources: %v", err)
		return err
	}
	srcObjects, _, err := m.getResources(
		migrationNamespaces,
		migration,
		migration.Spec.Selectors,
		migration.Spec.ExcludeSelectors,
		resourceCollectorOpts,
		false,
	)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error getting resources from source: %v", err))
		log.MigrationLog(migration).Errorf("Error getting resources: %v", err)
		return err
	}
	obj, err := objectToCollect(destObjects)
	if err != nil {
		return err
	}
	toBeDeleted := m.resourceCollector.ObjectTobeDeleted(srcObjects, obj)
	dynamicInterface, err := dynamic.NewForConfig(remoteConfig)
	if err != nil {
		return err
	}
	err = m.resourceCollector.DeleteResources(dynamicInterface, toBeDeleted, nil)
	if err != nil {
		return err
	}

	// update status of cleaned up objects migration info
	for _, r := range toBeDeleted {
		nm, ns, kind, err := utils.GetObjectDetails(r)
		if err != nil {
			// log error and skip adding object to status
			log.MigrationLog(migration).Errorf("Unable to get object details: %v", err)
			continue
		}
		resourceInfo := &stork_api.MigrationResourceInfo{
			Name:      nm,
			Namespace: ns,
			Status:    stork_api.MigrationStatusPurged,
		}
		resourceInfo.Kind = kind
		migration.Status.Resources = append(migration.Status.Resources, resourceInfo)
	}

	return nil
}

func objectToCollect(destObject []runtime.Unstructured) ([]runtime.Unstructured, error) {
	var objects []runtime.Unstructured
	for _, obj := range destObject {
		metadata, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		if metadata.GetNamespace() != "" {
			if val, ok := metadata.GetAnnotations()[StorkMigrationAnnotation]; ok {
				if skip, err := strconv.ParseBool(val); err == nil && skip {
					objects = append(objects, obj)
				}
			}
		}
	}
	return objects, nil
}

func (m *MigrationController) namespaceMigrationAllowed(migration *stork_api.Migration, migrationNamespaces []string) bool {
	// Restrict migration to only the namespace that the object belongs
	// except for the namespace designated by the admin
	if migration.Namespace != m.migrationAdminNamespace {
		for _, ns := range migrationNamespaces {
			if ns != migration.Namespace {
				return false
			}
		}
	}
	return true
}

func (m *MigrationController) migrateVolumes(migration *stork_api.Migration, migrationNamespaces []string, terminationChannels []chan bool) error {
	defer func() {
		for _, channel := range terminationChannels {
			channel <- true
		}
	}()

	migration.Status.Stage = stork_api.MigrationStageVolumes
	// Trigger the migration if we don't have any status
	if migration.Status.Volumes == nil {
		// Make sure storage is ready in the cluster pair
		storageStatus, err := getClusterPairStorageStatus(
			migration.Spec.ClusterPair,
			migration.Namespace)
		if err != nil || storageStatus != stork_api.ClusterPairStatusReady {
			// If there was a preExecRule configured, reset the stage so that it
			// gets retriggered in the next cycle
			if migration.Spec.PreExecRule != "" {
				migration.Status.Stage = stork_api.MigrationStageInitial
				err := m.updateMigrationCR(context.TODO(), migration)
				if err != nil {
					return err
				}
			}
			return fmt.Errorf("cluster pair storage status is not ready. Status: %v Err: %v",
				storageStatus, err)
		}

		volumeInfos, err := m.volDriver.StartMigration(migration, migrationNamespaces)
		if err != nil {
			return err
		}
		if volumeInfos == nil {
			volumeInfos = make([]*stork_api.MigrationVolumeInfo, 0)
		}
		migration.Status.Volumes = volumeInfos
		migration.Status.Status = stork_api.MigrationStatusInProgress
		err = m.updateMigrationCR(context.TODO(), migration)
		if err != nil {
			return err
		}

		// Terminate any background rules that were started
		for _, channel := range terminationChannels {
			channel <- true
		}
		terminationChannels = nil

		// Run any post exec rules once migration is triggered
		if migration.Spec.PostExecRule != "" {
			err = m.runPostExecRule(migration, migrationNamespaces)
			if err != nil {
				message := fmt.Sprintf("Error running PostExecRule: %v", err)
				log.MigrationLog(migration).Errorf(message)
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(stork_api.MigrationStatusFailed),
					message)

				// Cancel the migration and mark it as failed if the postExecRule failed
				err = m.volDriver.CancelMigration(migration)
				if err != nil {
					log.MigrationLog(migration).Errorf("Error cancelling migration: %v", err)
				}
				migration.Status.Stage = stork_api.MigrationStageFinal
				migration.Status.FinishTimestamp = metav1.Now()
				migration.Status.Status = stork_api.MigrationStatusFailed
				err = m.updateMigrationCR(context.TODO(), migration)
				if err != nil {
					return err
				}
				return fmt.Errorf("%v", message)
			}
		}
	}

	inProgress := false
	// Skip checking status if no volumes are being migrated
	if len(migration.Status.Volumes) != 0 {
		// Now check the status
		volumeInfos, err := m.volDriver.GetMigrationStatus(migration)
		if err != nil {
			return err
		}
		if volumeInfos == nil {
			volumeInfos = make([]*stork_api.MigrationVolumeInfo, 0)
		}
		migration.Status.Volumes = volumeInfos
		// Store the new status
		err = m.updateMigrationCR(context.TODO(), migration)
		if err != nil {
			return err
		}

		// Now check if there is any failure or success
		// TODO: On failure of one volume cancel other migrations?
		for _, vInfo := range volumeInfos {
			if vInfo.Status == stork_api.MigrationStatusInProgress {
				log.MigrationLog(migration).Infof("Volume migration still in progress: %v", vInfo.Volume)
				inProgress = true
			} else if vInfo.Status == stork_api.MigrationStatusFailed {
				m.recorder.Event(migration,
					v1.EventTypeWarning,
					string(vInfo.Status),
					fmt.Sprintf("Error migrating volume %v: %v", vInfo.Volume, vInfo.Reason))
				migration.Status.Stage = stork_api.MigrationStageFinal
				migration.Status.FinishTimestamp = metav1.Now()
				migration.Status.Status = stork_api.MigrationStatusFailed
			} else if vInfo.Status == stork_api.MigrationStatusSuccessful {
				m.recorder.Event(migration,
					v1.EventTypeNormal,
					string(vInfo.Status),
					fmt.Sprintf("Volume %v migrated successfully", vInfo.Volume))
			}
		}
	}

	// Return if we have any volume migrations still in progress
	if inProgress {
		return nil
	}

	migration.Status.VolumeMigrationFinishTimestamp = metav1.Now()
	// If the migration hasn't failed move on to the next stage.
	if migration.Status.Status != stork_api.MigrationStatusFailed {
		if *migration.Spec.IncludeResources {
			migration.Status.Stage = stork_api.MigrationStageApplications
			migration.Status.Status = stork_api.MigrationStatusInProgress
			// Update the current state and then move on to migrating
			// resources
			err := m.updateMigrationCR(context.TODO(), migration)
			if err != nil {
				return err
			}
			err = m.migrateResources(migration, migrationNamespaces, false)
			if err != nil {
				log.MigrationLog(migration).Errorf("Error migrating resources: %v", err)
				return err
			}
		} else {
			err := m.migrateResources(migration, migrationNamespaces, true)
			if err != nil {
				log.MigrationLog(migration).Errorf("Error migrating resources: %v", err)
				return err
			}
			migration.Status.Stage = stork_api.MigrationStageFinal
			migration.Status.FinishTimestamp = metav1.Now()
			migration.Status.Status = stork_api.MigrationStatusSuccessful
		}
	}

	return m.updateMigrationCR(context.TODO(), migration)
}

func (m *MigrationController) runPreExecRule(migration *stork_api.Migration, migrationNamespaces []string) ([]chan bool, error) {
	if migration.Spec.PreExecRule == "" {
		migration.Status.Stage = stork_api.MigrationStageVolumes
		migration.Status.Status = stork_api.MigrationStatusPending
		err := m.updateMigrationCR(context.TODO(), migration)
		if err != nil {
			return nil, err
		}
		return nil, nil
	} else if migration.Status.Stage == stork_api.MigrationStageInitial {
		migration.Status.Stage = stork_api.MigrationStagePreExecRule
		migration.Status.Status = stork_api.MigrationStatusPending
	}

	if migration.Status.Stage == stork_api.MigrationStagePreExecRule {
		if migration.Status.Status == stork_api.MigrationStatusPending {
			migration.Status.Status = stork_api.MigrationStatusInProgress
			err := m.updateMigrationCR(context.TODO(), migration)
			if err != nil {
				return nil, err
			}
		} else if migration.Status.Status == stork_api.MigrationStatusInProgress {
			m.recorder.Event(migration,
				v1.EventTypeNormal,
				string(stork_api.MigrationStatusInProgress),
				fmt.Sprintf("Waiting for PreExecRule %v", migration.Spec.PreExecRule))
			return nil, nil
		}
	}
	r, err := storkops.Instance().GetRule(migration.Spec.PreExecRule, migration.Namespace)
	if err != nil {
		return nil, err
	}
	terminationChannels := make([]chan bool, 0)
	for _, ns := range migrationNamespaces {
		ch, err := rule.ExecuteRule(r, rule.PreExecRule, migration, ns)
		if err != nil {
			for _, channel := range terminationChannels {
				channel <- true
			}
			return nil, fmt.Errorf("error executing PreExecRule for namespace %v: %v", ns, err)
		}
		if ch != nil {
			terminationChannels = append(terminationChannels, ch)
		}
	}
	return terminationChannels, nil
}

func (m *MigrationController) runPostExecRule(migration *stork_api.Migration, migrationNamespaces []string) error {
	r, err := storkops.Instance().GetRule(migration.Spec.PostExecRule, migration.Namespace)
	if err != nil {
		return err
	}
	for _, ns := range migrationNamespaces {
		_, err = rule.ExecuteRule(r, rule.PostExecRule, migration, ns)
		if err != nil {
			return fmt.Errorf("error executing PostExecRule for namespace %v: %v", ns, err)
		}
	}
	return nil
}

func (m *MigrationController) migrateResources(migration *stork_api.Migration, migrationNamespaces []string, volumesOnly bool) error {
	clusterPair, err := storkops.Instance().GetClusterPair(migration.Spec.ClusterPair, migration.Namespace)
	if err != nil {
		return err
	}
	schedulerStatus := clusterPair.Status.SchedulerStatus

	if schedulerStatus != stork_api.ClusterPairStatusReady {
		return fmt.Errorf("scheduler Cluster pair is not ready. Status: %v", schedulerStatus)
	}

	if migration.Spec.AdminClusterPair != "" {
		schedulerStatus, err = getClusterPairSchedulerStatus(migration.Spec.AdminClusterPair, m.migrationAdminNamespace)
		if err != nil {
			return err
		}
		if schedulerStatus != stork_api.ClusterPairStatusReady {
			return fmt.Errorf("scheduler in Admin Cluster pair is not ready. Status: %v", schedulerStatus)
		}
	}
	resGroups := make(map[string]string)
	var updateObjects, allObjects []runtime.Unstructured

	var pvcsWithOwnerRef []v1.PersistentVolumeClaim
	// Don't modify resources if mentioned explicitly in specs
	resourceCollectorOpts := resourcecollector.Options{}
	if *migration.Spec.SkipServiceUpdate {
		resourceCollectorOpts.SkipServices = true
	}
	if clusterPair.Spec.PlatformOptions.Rancher != nil && len(clusterPair.Spec.PlatformOptions.Rancher.ProjectMappings) > 0 {
		resourceCollectorOpts.RancherProjectMappings = make(map[string]string)
		for k, v := range clusterPair.Spec.PlatformOptions.Rancher.ProjectMappings {
			resourceCollectorOpts.RancherProjectMappings[k] = v
		}
	}
	if *migration.Spec.IncludeNetworkPolicyWithCIDR {
		resourceCollectorOpts.IncludeAllNetworkPolicies = true
	}
	if *migration.Spec.IgnoreOwnerReferencesCheck {
		resourceCollectorOpts.IgnoreOwnerReferencesCheck = true
	}
	if volumesOnly {
		allObjects, pvcsWithOwnerRef, err = m.getVolumeOnlyMigrationResources(migration, migrationNamespaces, resourceCollectorOpts)
		if err != nil {
			// already raised event in getVolumeOnlyMigrationResources()
			return err
		}
	} else {
		allObjects, pvcsWithOwnerRef, err = m.getResources(
			migrationNamespaces,
			migration,
			migration.Spec.Selectors,
			migration.Spec.ExcludeSelectors,
			resourceCollectorOpts,
			false,
		)

		if err != nil {
			m.recorder.Event(migration,
				v1.EventTypeWarning,
				string(stork_api.MigrationStatusFailed),
				fmt.Sprintf("Error getting resource: %v", err))
			log.MigrationLog(migration).Errorf("Error getting resources: %v", err)
			return err
		}
	}

	// Save the collected resources infos in the status
	resourceInfos := make([]*stork_api.MigrationResourceInfo, 0)
	for _, obj := range allObjects {
		metadata, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		gvk := obj.GetObjectKind().GroupVersionKind()
		if volumesOnly {
			switch gvk.Kind {
			case "PersistentVolume":
			case "PersistentVolumeClaim":
			default:
				continue
			}
		}
		resourceInfo := &stork_api.MigrationResourceInfo{
			Name:      metadata.GetName(),
			Namespace: metadata.GetNamespace(),
			Status:    stork_api.MigrationStatusInProgress,
		}

		resourceInfo.Kind = gvk.Kind
		resourceInfo.Group = gvk.Group
		// core Group doesn't have a name, so override it
		if resourceInfo.Group == "" {
			resourceInfo.Group = "core"
		}
		resourceInfo.Version = gvk.Version
		resGroups[gvk.Group] = gvk.Version
		resourceInfos = append(resourceInfos, resourceInfo)
		updateObjects = append(updateObjects, obj)
	}

	migration.Status.Resources = resourceInfos
	err = m.updateMigrationCR(context.TODO(), migration)
	if err != nil {
		return err
	}

	crdList, err := storkcache.Instance().ListApplicationRegistrations()
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error applying resource: %v", err))
		log.MigrationLog(migration).Errorf("Error applying resources: %v", err)
		return err
	}

	err = m.prepareResources(migration, updateObjects, clusterPair, crdList)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error preparing resource: %v", err))
		log.MigrationLog(migration).Errorf("Error preparing resources: %v", err)
		return err
	}

	err = m.applyResources(migration, migrationNamespaces, updateObjects, resGroups, clusterPair, crdList)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error applying resource: %v", err))
		log.MigrationLog(migration).Errorf("Error applying resources: %v", err)
		return err
	}

	err = m.updateOwnerReferenceOnPVC(migration, updateObjects, clusterPair, pvcsWithOwnerRef, crdList)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error updating PVC resource: %v", err))
		log.MigrationLog(migration).Errorf("Error updating PVC resource:: %v", err)
		return err
	}

	err = m.updateStorageClassOnPV(migration, updateObjects, clusterPair)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error updating StorageClass on PV: %v", err))
		log.MigrationLog(migration).Errorf("Error updating Storageclass for PV: %v", err)
		return err
	}

	migration.Status.Stage = stork_api.MigrationStageFinal
	migration.Status.Status = stork_api.MigrationStatusSuccessful
	for _, resource := range migration.Status.Resources {
		if resource.Status != stork_api.MigrationStatusSuccessful {
			migration.Status.Status = stork_api.MigrationStatusPartialSuccess
			break
		}
	}

	if *migration.Spec.PurgeDeletedResources {
		if err := m.purgeMigratedResources(migration, migrationNamespaces, resourceCollectorOpts); err != nil {
			message := fmt.Sprintf("Error cleaning up resources: %v", err)
			log.MigrationLog(migration).Errorf(message)
			m.recorder.Event(migration,
				v1.EventTypeWarning,
				string(stork_api.MigrationStatusPartialSuccess),
				message)
			return nil
		}
	}

	migration.Status.ResourceMigrationFinishTimestamp = metav1.Now()
	migration.Status.FinishTimestamp = metav1.Now()
	err = m.updateMigrationCR(context.TODO(), migration)
	if err != nil {
		return err
	}
	return nil
}

func (m *MigrationController) prepareResources(
	migration *stork_api.Migration,
	objects []runtime.Unstructured,
	clusterPair *stork_api.ClusterPair,
	crdList *stork_api.ApplicationRegistrationList,
) error {
	transformName := ""
	// this is already handled in pre-checks, we dont support multiple resource transformation
	// rules specified in migration specs
	if len(migration.Spec.TransformSpecs) != 0 && len(migration.Spec.TransformSpecs) == 1 {
		transformName = migration.Spec.TransformSpecs[0]
	}

	resPatch := make(map[string]stork_api.KindResourceTransform)
	var err error
	if transformName != "" {
		resPatch, err = resourcecollector.GetResourcePatch(transformName, migration.Spec.Namespaces)
		if err != nil {
			log.MigrationLog(migration).
				Warnf("Unable to get transformation spec from :%s, skipping transformation for this migration, err: %v", transformName, err)
			return err
		}
	}

	for _, o := range objects {
		metadata, err := meta.Accessor(o)
		if err != nil {
			return err
		}
		resource := o.GetObjectKind().GroupVersionKind()
		switch resource.Kind {
		case "PersistentVolume":
			err := m.preparePVResource(migration, o)
			if err != nil {
				return fmt.Errorf("error preparing PV resource %v: %v", metadata.GetName(), err)
			}
		case "Deployment", "StatefulSet", "DeploymentConfig", "IBPPeer", "IBPCA", "IBPConsole", "IBPOrderer", "ReplicaSet":
			err := m.prepareApplicationResource(migration, clusterPair, o)
			if err != nil {
				return fmt.Errorf("error preparing %v resource %v: %v", o.GetObjectKind().GroupVersionKind().Kind, metadata.GetName(), err)
			}
		case "CronJob":
			err := m.prepareJobResource(migration, o)
			if err != nil {
				return fmt.Errorf("error preparing %v resource %v: %v", o.GetObjectKind().GroupVersionKind().Kind, metadata.GetName(), err)
			}
		default:
			// if namespace has resource transformation spec
			if ns, found := resPatch[metadata.GetNamespace()]; found {
				// if transformspec present for current resource kind
				if kind, ok := ns[resource.Kind]; ok {
					err := resourcecollector.TransformResources(o, kind, metadata.GetName(), metadata.GetNamespace())
					if err != nil {
						return fmt.Errorf("error updating %v resource %v: %v", o.GetObjectKind().GroupVersionKind().Kind, metadata.GetName(), err)
					}
				}
			}
			// do nothing
		}

		// prepare CR resources
		for _, crd := range crdList.Items {
			for _, v := range crd.Resources {
				if v.Kind == resource.Kind &&
					v.Version == resource.Version &&
					v.Group == resource.Group {
					v.NestedSuspendOptions = append(v.NestedSuspendOptions, v.SuspendOptions)
					if err := m.prepareCRDClusterResource(migration, o, v.NestedSuspendOptions); err != nil {
						return fmt.Errorf("error preparing %v resource %v: %v",
							o.GetObjectKind().GroupVersionKind().Kind, metadata.GetName(), err)
					}
				}
			}
		}
	}
	return nil
}

func (m *MigrationController) updateResourceStatus(
	migration *stork_api.Migration,
	object runtime.Unstructured,
	status stork_api.MigrationStatusType,
	reason string,
) {
	for _, resource := range migration.Status.Resources {
		metadata, err := meta.Accessor(object)
		if err != nil {
			continue
		}
		gkv := object.GetObjectKind().GroupVersionKind()
		if resource.Name == metadata.GetName() &&
			resource.Namespace == metadata.GetNamespace() &&
			(resource.Group == gkv.Group || (resource.Group == "core" && gkv.Group == "")) &&
			resource.Version == gkv.Version &&
			resource.Kind == gkv.Kind {
			if _, ok := metadata.GetAnnotations()[resourcecollector.TransformedResourceName]; ok {
				if len(migration.Spec.TransformSpecs) != 0 && len(migration.Spec.TransformSpecs) == 1 {
					resource.TransformedBy = migration.Spec.TransformSpecs[0]
				}
			}
			resource.Status = status
			resource.Reason = reason
			eventType := v1.EventTypeNormal
			if status == stork_api.MigrationStatusFailed {
				eventType = v1.EventTypeWarning
			}
			eventMessage := fmt.Sprintf("%v %v/%v: %v",
				gkv,
				resource.Namespace,
				resource.Name,
				reason)
			m.recorder.Event(migration, eventType, string(status), eventMessage)
			return
		}
	}
}

func (m *MigrationController) getRemoteClient(migration *stork_api.Migration) (*RemoteClient, error) {
	remoteConfig, err := getClusterPairSchedulerConfig(migration.Spec.ClusterPair, migration.Namespace)
	if err != nil {
		return nil, err
	}
	remoteAdminConfig := remoteConfig
	// Use the admin cluter pair for cluster scoped resources if it has been configured
	if migration.Spec.AdminClusterPair != "" {
		remoteAdminConfig, err = getClusterPairSchedulerConfig(migration.Spec.AdminClusterPair, m.migrationAdminNamespace)
		if err != nil {
			return nil, err
		}
	}
	adminClient, err := kubernetes.NewForConfig(remoteAdminConfig)
	if err != nil {
		return nil, err
	}
	remoteInterface, err := dynamic.NewForConfig(remoteConfig)
	if err != nil {
		return nil, err
	}
	remoteAdminInterface := remoteInterface
	if migration.Spec.AdminClusterPair != "" {
		remoteAdminInterface, err = dynamic.NewForConfig(remoteAdminConfig)
		if err != nil {
			return nil, err
		}
	}
	remoteClient := RemoteClient{
		remoteConfig:         remoteConfig,
		remoteAdminConfig:    remoteAdminConfig,
		adminClient:          adminClient,
		remoteInterface:      remoteInterface,
		remoteAdminInterface: remoteAdminInterface,
	}
	return &remoteClient, err
}

func (m *MigrationController) isServiceUpdated(
	migration *stork_api.Migration,
	object runtime.Unstructured,
	objHash uint64,
) (bool, error) {
	var svc v1.Service
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.UnstructuredContent(), &svc); err != nil {
		return false, fmt.Errorf("error converting unstructured obj to service resource: %v", err)
	}
	if _, ok := svc.Annotations[resourcecollector.SkipModifyResources]; ok {
		// older behaviour where we delete and create svc resources
		return false, nil
	}
	remoteClient, err := m.getRemoteClient(migration)
	if err != nil {
		return false, err
	}

	// compare and decide if update to svc is required on dest cluster
	curr, err := remoteClient.adminClient.CoreV1().Services(svc.GetNamespace()).Get(context.TODO(), svc.Name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if curr.Annotations == nil {
		curr.Annotations = make(map[string]string)
	}
	if hash, ok := curr.Annotations[resourcecollector.StorkResourceHash]; ok {
		old, err := strconv.ParseUint(hash, 10, 64)
		if err != nil {
			return false, err
		}
		if old == objHash {
			log.MigrationLog(migration).Infof("skipping service update, no changes found since last migration %d/%d", old, objHash)
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

func (m *MigrationController) secretToBeMigrated(name string) bool {
	for _, prefix := range AutoCreatedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func (m *MigrationController) checkAndUpdateDefaultSA(
	migration *stork_api.Migration,
	object runtime.Unstructured,
) error {
	var sourceSA v1.ServiceAccount
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.UnstructuredContent(), &sourceSA); err != nil {
		return fmt.Errorf("error converting to serviceAccount: %v", err)
	}
	remoteClient, err := m.getRemoteClient(migration)
	if err != nil {
		return err
	}

	log.MigrationLog(migration).Infof("Updating service account(namespace/name : %s/%s) with image pull secrets", sourceSA.GetNamespace(), sourceSA.GetName())
	// merge service account resource for default namespaces
	destSA, err := remoteClient.adminClient.CoreV1().ServiceAccounts(sourceSA.GetNamespace()).Get(context.TODO(), sourceSA.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, s := range sourceSA.ImagePullSecrets {
		found := false
		for _, d := range destSA.ImagePullSecrets {
			if d.Name == s.Name {
				found = true
				break
			}
		}
		if !found {
			destSA.ImagePullSecrets = append(destSA.ImagePullSecrets, s)
		}
	}

	for _, s := range sourceSA.Secrets {
		if !m.secretToBeMigrated(s.Name) {
			continue
		}
		found := false
		for _, d := range destSA.Secrets {
			if d.Name == s.Name {
				found = true
				break
			}
		}
		if !found {
			destSA.Secrets = append(destSA.Secrets, s)
		}
	}
	destSA.AutomountServiceAccountToken = sourceSA.AutomountServiceAccountToken

	// merge annotation for SA
	log.MigrationLog(migration).Infof("Updating service account(namespace/name : %s/%s) annotations", sourceSA.GetNamespace(), sourceSA.GetName())
	if destSA.Annotations != nil {
		if sourceSA.Annotations == nil {
			sourceSA.Annotations = make(map[string]string)
		}
		for k, v := range sourceSA.Annotations {
			destSA.Annotations[k] = v
		}
	}
	_, err = remoteClient.adminClient.CoreV1().ServiceAccounts(destSA.GetNamespace()).Update(context.TODO(), destSA, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (m *MigrationController) preparePVResource(
	migration *stork_api.Migration,
	object runtime.Unstructured,
) error {
	var pv v1.PersistentVolume
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.UnstructuredContent(), &pv); err != nil {
		return err
	}
	// lets keep retain policy always before applying migration
	if pv.Annotations == nil {
		pv.Annotations = make(map[string]string)
	}
	pv.Annotations[PVReclaimAnnotation] = string(pv.Spec.PersistentVolumeReclaimPolicy)
	pv.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimRetain
	_, err := m.volDriver.UpdateMigratedPersistentVolumeSpec(&pv, nil, nil, "", "")
	if err != nil {
		return err
	}
	o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pv)
	if err != nil {
		return err
	}
	object.SetUnstructuredContent(o)

	return nil
}

// this method can be used for k8s object where we will need to set resource to true/false to disable them
// on migration
func (m *MigrationController) prepareJobResource(
	migration *stork_api.Migration,
	object runtime.Unstructured,
) error {
	if *migration.Spec.StartApplications {
		return nil
	}
	content := object.UnstructuredContent()
	// set suspend to true to disable Cronjobs
	return unstructured.SetNestedField(content, true, "spec", "suspend")
}

func (m *MigrationController) prepareApplicationResource(
	migration *stork_api.Migration,
	clusterPair *stork_api.ClusterPair,
	object runtime.Unstructured,
) error {
	content := object.UnstructuredContent()
	if clusterPair.Spec.PlatformOptions.Rancher != nil &&
		len(clusterPair.Spec.PlatformOptions.Rancher.ProjectMappings) > 0 {
		podSpecField, found, err := unstructured.NestedFieldCopy(content, "spec", "template", "spec")
		if err != nil {
			logrus.Warnf("Unable to parse object %v while handling"+
				" rancher project mappings", object.GetObjectKind().GroupVersionKind().Kind)
		}
		podSpec, ok := podSpecField.(v1.PodSpec)
		if found && ok {
			podSpecPtr := resourcecollector.PreparePodSpecNamespaceSelector(
				&podSpec,
				clusterPair.Spec.PlatformOptions.Rancher.ProjectMappings,
			)
			if err := unstructured.SetNestedField(content, *podSpecPtr, "spec", "template", "spec"); err != nil {
				logrus.Warnf("Unable to set namespace selector for object %v while handling"+
					" rancher project mappings", object.GetObjectKind().GroupVersionKind().Kind)
			}
		}
	}

	// Reset the replicas to 0 and store the current replicas in an annotation
	replicas, found, err := unstructured.NestedInt64(content, "spec", "replicas")
	if err != nil {
		return err
	}
	if !found {
		replicas = 1
	}

	annotations, found, err := unstructured.NestedStringMap(content, "metadata", "annotations")
	if err != nil {
		return err
	}
	if !found {
		annotations = make(map[string]string)
	}
	migrationReplicasAnnotationValue, replicaAnnotationIsPresent := annotations[StorkMigrationReplicasAnnotation]

	if replicas == 0 && replicaAnnotationIsPresent {
		//Only if the actual replica count is 0 and migrationReplicas annotation is more than 0, then carry forward the annotation.
		migrationReplicas, err := strconv.ParseInt(migrationReplicasAnnotationValue, 10, 64)
		if err != nil {
			return err
		}
		if migrationReplicas > 0 {
			//carry forward the migrationReplica value
			replicas = migrationReplicas
			//we reset the actual replica count as well in case the migration has startApplications enabled
			err = unstructured.SetNestedField(content, replicas, "spec", "replicas")
			if err != nil {
				return err
			}
		}
	}

	//No need to scale down if StartApplications is set to True in the migration spec
	if *migration.Spec.StartApplications {
		return nil
	}

	//we set the actual replica count to 0 to scale down the resource on target cluster
	err = unstructured.SetNestedField(content, int64(0), "spec", "replicas")
	if err != nil {
		return err
	}

	labels, found, err := unstructured.NestedStringMap(content, "metadata", "labels")
	if err != nil {
		return err
	}
	if !found {
		labels = make(map[string]string)
	}
	labels[StorkMigrationAnnotation] = "true"
	if err := unstructured.SetNestedStringMap(content, labels, "metadata", "labels"); err != nil {
		return err
	}

	annotations[StorkMigrationReplicasAnnotation] = strconv.FormatInt(replicas, 10)
	return unstructured.SetNestedStringMap(content, annotations, "metadata", "annotations")
}

func (m *MigrationController) prepareCRDClusterResource(
	migration *stork_api.Migration,
	object runtime.Unstructured,
	suspendOpts []stork_api.SuspendOptions,
) error {
	if len(suspendOpts) == 0 {
		return nil
	}
	content := object.UnstructuredContent()
	annotations, found, err := unstructured.NestedStringMap(content, "metadata", "annotations")
	if err != nil {
		return err
	}
	if !found {
		annotations = make(map[string]string)
	}
	for _, suspend := range suspendOpts {
		fields := strings.Split(suspend.Path, ".")
		var currVal string
		if len(fields) > 1 {
			SuspendAnnotationValue, suspendAnnotationIsPresent := annotations[StorkAnnotationPrefix+suspend.Path]
			var disableVersion interface{}
			if suspend.Type == "bool" {
				if val, err := strconv.ParseBool(suspend.Value); err != nil {
					disableVersion = true
				} else {
					disableVersion = val
				}
			} else if suspend.Type == "int" {
				curr, found, err := unstructured.NestedInt64(content, fields...)
				if err != nil || !found {
					return fmt.Errorf("unable to find suspend path, err: %v", err)
				}
				disableVersion = int64(0)
				if curr == 0 && suspendAnnotationIsPresent {
					//suspendAnnotation has value set as {currVal + "," + suspend.Value}
					//we need to extract only the currVal from it
					annotationValue := strings.Split(SuspendAnnotationValue, ",")[0]
					intValue, err := strconv.ParseInt(annotationValue, 10, 64)
					if err != nil {
						return err
					}
					if intValue > 0 {
						//Only if the actual suspend path value is 0 and suspend annotation value is more than 0,
						//then carry forward the annotation.
						curr = intValue
						//we reset the actual suspend path value as well in case the migration has startApplications enabled
						err = unstructured.SetNestedField(content, fmt.Sprintf("%v", curr), fields...)
						if err != nil {
							return err
						}
					}
				}
				currVal = fmt.Sprintf("%v", curr)
			} else if suspend.Type == "string" {
				curr, _, err := unstructured.NestedString(content, fields...)
				if err != nil {
					return fmt.Errorf("unable to find suspend path, err: %v", err)
				}
				disableVersion = suspend.Value
				if curr == suspend.Value && suspendAnnotationIsPresent {
					annotationValue := strings.Split(SuspendAnnotationValue, ",")[0]
					if annotationValue != "" {
						//Only if the actual value is equal to suspend.Value and suspend path annotation value is not an empty string,
						//then carry forward the annotation.
						curr = annotationValue
						err = unstructured.SetNestedField(content, curr, fields...)
						if err != nil {
							return err
						}
					}
				}
				currVal = curr
			} else {
				return fmt.Errorf("invalid type %v to suspend cr", suspend.Type)
			}

			//No need to scale down if StartApplications is set to True in the migration spec
			if *migration.Spec.StartApplications {
				return nil
			}

			//scale down the CRD resource by setting the suspendPath value to disableVersion
			if err := unstructured.SetNestedField(content, disableVersion, fields...); err != nil {
				return err
			}

			// path : activate/deactivate value
			annotations[StorkAnnotationPrefix+suspend.Path] = currVal + "," + suspend.Value
		}
	}

	if *migration.Spec.StartApplications {
		return nil
	}

	return unstructured.SetNestedStringMap(content, annotations, "metadata", "annotations")
}

func (m *MigrationController) getParsedMap(
	inputMap map[string]string,
	clusterPair *stork_api.ClusterPair,
) map[string]string {
	if inputMap == nil {
		return nil
	}
	a := make(map[string]string)
	for k, v := range inputMap {
		if !strings.Contains(k, utils.CattlePrefix) {
			a[k] = v
		} else {
			if strings.Contains(k, utils.CattleProjectPrefix) {
				if clusterPair.Spec.PlatformOptions.Rancher != nil {
					if targetProjectID, ok := clusterPair.Spec.PlatformOptions.Rancher.ProjectMappings[v]; ok &&
						targetProjectID != "" {
						a[k] = targetProjectID
					} // else skip the project label
				} // else skip the project label
			} // skip all the other cattle label
		}
	}
	return a
}

func (m *MigrationController) getParsedAnnotations(
	annotations map[string]string,
	clusterPair *stork_api.ClusterPair,
) map[string]string {
	return m.getParsedMap(annotations, clusterPair)
}

func (m *MigrationController) getParsedLabels(
	labels map[string]string,
	clusterPair *stork_api.ClusterPair,
) map[string]string {
	return m.getParsedMap(labels, clusterPair)
}

// updateStorageClassOnPV updates StorageClass on PV object
// StorageClass is created on destination if it doesn't exist
func (m *MigrationController) updateStorageClassOnPV(
	migration *stork_api.Migration,
	objects []runtime.Unstructured,
	clusterPair *stork_api.ClusterPair,
) error {
	remoteClient, err := m.getRemoteClient(migration)
	if err != nil {
		return err
	}
	// Get a list of StorageClasses from destination
	destStorageClasses, err := remoteClient.adminClient.StorageV1().StorageClasses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	destScExists := make(map[string]bool)
	for _, sc := range destStorageClasses.Items {
		destScExists[sc.Name] = true
	}

	for _, obj := range objects {
		metadata, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		if obj.GetObjectKind().GroupVersionKind().Kind != "PersistentVolume" {
			continue
		}
		destPV, err := remoteClient.adminClient.CoreV1().PersistentVolumes().Get(context.TODO(), metadata.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if scName, ok := destPV.Annotations[resourcecollector.CurrentStorageClassName]; ok {
			// Create StorageClass on destination if it doesn't exist
			if _, ok := destScExists[scName]; !ok {
				// Get StorageClass from source
				sc, err := storkcache.Instance().GetStorageClass(scName)
				if err != nil {
					return err
				}
				sc.UID = ""
				sc.ResourceVersion = ""
				log.MigrationLog(migration).Infof("Applying %v %v", sc.Kind, sc.Name)
				for retries := 0; retries < maxApplyRetries; retries++ {
					_, err = remoteClient.adminClient.StorageV1().StorageClasses().Create(context.TODO(), sc, metav1.CreateOptions{})
					if err == nil || errors.IsAlreadyExists(err) {
						// Update the map to reflect the new StorageClass that was created on destination
						destScExists[scName] = true
						break
					}
					log.MigrationLog(migration).Infof("Unable to create %v %v on destination due to err: %v. Retrying.", sc.Kind, sc.Name, err)
					time.Sleep(applyRetryInterval)
				}
				if err != nil {
					log.MigrationLog(migration).Errorf("All attempts to create %v %v on destination failed due to err: %v", sc.Kind, sc.Name, err)
					return err
				}
			}
			// IF StorageClass is already updated on destination PV, then no need to update
			// It is possible that StorageClass was deleted manually on destination cluster while PV was present with StorageClass name
			// To make sure such inconsistencies get fixed in subsequent migration, this check is being done after StorageClass creation
			if scName == destPV.Spec.StorageClassName {
				continue
			}
			// Update StorageClass on destination PV
			destPV.Spec.StorageClassName = scName
			log.MigrationLog(migration).Infof("Updating %v %v with StorageClass %v", destPV.Kind, destPV.Name, scName)
			if _, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Update(context.TODO(), destPV, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateOwnerReferenceOnPVC updates owner reference on PVC objects
// on destination cluster
func (m *MigrationController) updateOwnerReferenceOnPVC(
	migration *stork_api.Migration,
	objects []runtime.Unstructured,
	clusterPair *stork_api.ClusterPair,
	pvcsWithOwnerRef []v1.PersistentVolumeClaim,
	crdList *stork_api.ApplicationRegistrationList,
) error {
	remoteClient, err := m.getRemoteClient(migration)
	if err != nil {
		return err
	}

	appRegsStashMap := make(map[string]bool)
	if !*migration.Spec.StartApplications {
		appRegsStashMap = getAppRegsStashMap(*crdList)
	}

	ruleset := resourcecollector.GetDefaultRuleSet()
	for _, srcPvc := range pvcsWithOwnerRef {
		destPvc, err := remoteClient.adminClient.CoreV1().PersistentVolumeClaims(srcPvc.GetNamespace()).Get(context.TODO(), srcPvc.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		destPvcOwnersList := make([]metav1.OwnerReference, 0)

		// This loop finds the owner reference for the current pvc owner
		for _, pvcOwner := range srcPvc.GetOwnerReferences() {
			var dynamicClient dynamic.ResourceInterface
			ownerName := ""
			for _, o := range objects {
				metadata, err := meta.Accessor(o)
				if err != nil {
					return err
				}
				objectType, err := meta.TypeAccessor(o)
				if err != nil {
					return err
				}
				// Found the source object which is the parent of the source pvc resource
				if metadata.GetName() == pvcOwner.Name && objectType.GetKind() == pvcOwner.Kind {
					gvk := o.GetObjectKind().GroupVersionKind()
					keyName := getAppRegsStashMapKeyName(gvk.Group, gvk.Kind, gvk.Version)
					// if stashCR is enabled for the owner object, ignore updating ownerreference in pvc.
					// instead keep the information of the pvc to be updated in stashed cm
					// which will be used for updating ownerreference as part of storkctl activate.
					if appRegsStashMap[keyName] {
						cmName := utils.GetStashedConfigMapName(strings.ToLower(gvk.Kind), strings.ToLower(gvk.Group), metadata.GetName())
						err = updateStashedCMWithPVCInfo(remoteClient, cmName, metadata.GetNamespace(), destPvc.Name, pvcOwner)
						if err != nil {
							log.MigrationLog(migration).Errorf("updating stashed configmap %s failed with error: %v", cmName, err)
						}
						continue
					}
					ownerName = metadata.GetName()
					resource := &metav1.APIResource{
						Name:       ruleset.Pluralize(strings.ToLower(objectType.GetKind())),
						Namespaced: len(metadata.GetNamespace()) > 0,
					}
					if resource.Namespaced {
						dynamicClient = remoteClient.remoteInterface.Resource(
							o.GetObjectKind().GroupVersionKind().GroupVersion().WithResource(resource.Name)).Namespace(metadata.GetNamespace())
					} else {
						dynamicClient = remoteClient.remoteAdminInterface.Resource(
							o.GetObjectKind().GroupVersionKind().GroupVersion().WithResource(resource.Name))
					}
					break
				}
			}

			if len(ownerName) == 0 {
				continue
			}
			destinationParentResourceUnstructured, err := dynamicClient.Get(context.TODO(), ownerName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			currOwner := metav1.OwnerReference{
				APIVersion:         pvcOwner.APIVersion,
				Kind:               pvcOwner.Kind,
				Name:               pvcOwner.Name,
				UID:                destinationParentResourceUnstructured.GetUID(),
				Controller:         pvcOwner.Controller,
				BlockOwnerDeletion: pvcOwner.BlockOwnerDeletion,
			}
			// There can be multiple owners for one resource. Update the current owner
			// details in the list of owner references
			destPvcOwnersList = append(destPvcOwnersList, currOwner)
		}
		if len(destPvcOwnersList) > 0 {
			destPvc.SetOwnerReferences(destPvcOwnersList)
			if _, err = remoteClient.adminClient.CoreV1().PersistentVolumeClaims(destPvc.GetNamespace()).Update(context.TODO(), destPvc, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *MigrationController) applyResources(
	migration *stork_api.Migration,
	migrationNamespaces []string,
	objects []runtime.Unstructured,
	resGroups map[string]string,
	clusterPair *stork_api.ClusterPair,
	crdList *stork_api.ApplicationRegistrationList,
) error {
	remoteClient, err := m.getRemoteClient(migration)
	if err != nil {
		return err
	}

	ruleset := resourcecollector.GetDefaultRuleSet()

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("error getting cluster config: %v", err)
	}

	srcClnt, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return err
	}
	destClnt, err := apiextensionsclient.NewForConfig(remoteClient.remoteAdminConfig)
	if err != nil {
		return err
	}

	relatedCRDList := getRelatedCRDListWRTGroupAndCategories(srcClnt, ruleset, crdList, resGroups)

	// create CRD on destination cluster
	for _, crd := range relatedCRDList {
		for _, v := range crd.Resources {
			// only create relevant crds on dest cluster
			crdName := ruleset.Pluralize(strings.ToLower(v.Kind)) + "." + v.Group
			crdvbeta1, err := srcClnt.ApiextensionsV1beta1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
			if err == nil {
				crdvbeta1.ResourceVersion = ""
				if _, regErr := destClnt.ApiextensionsV1beta1().CustomResourceDefinitions().Create(context.TODO(), crdvbeta1, metav1.CreateOptions{}); regErr != nil && !errors.IsAlreadyExists(regErr) {
					log.MigrationLog(migration).Warnf("error registering crds %s, %v", crdvbeta1.GetName(), err)
				} else if regErr == nil {
					if err := k8sutils.ValidateCRD(destClnt, crdName); err != nil {
						log.MigrationLog(migration).Errorf("Unable to validate crds %v,%v", crdvbeta1.GetName(), err)
					}
					continue
				}
			}
			res, err := srcClnt.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					log.MigrationLog(migration).Warnf("CRDV1 not found %v for kind %v", crdName, v.Kind)
					continue
				}
				log.MigrationLog(migration).Errorf("unable to get customresourcedefination for %s, err: %v", crdName, err)
				return err
			}

			res.ResourceVersion = ""
			// if crds is applied as v1beta on k8s version 1.16+ it will have
			// preservedUnknownField set and api version converted to v1 ,
			// which cause issue while applying it on dest cluster,
			// since we will be applying v1 crds with non-valid schema

			// this converts `preserveUnknownFields`(deprecated) to spec.Versions[*].xPreservedUnknown
			// equivalent
			var updatedVersions []apiextensionsv1.CustomResourceDefinitionVersion
			if res.Spec.PreserveUnknownFields {
				res.Spec.PreserveUnknownFields = false
				for _, version := range res.Spec.Versions {
					isTrue := true
					if version.Schema == nil {
						openAPISchema := &apiextensionsv1.CustomResourceValidation{
							OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{XPreserveUnknownFields: &isTrue},
						}
						version.Schema = openAPISchema
					} else {
						version.Schema.OpenAPIV3Schema.XPreserveUnknownFields = &isTrue
					}
					updatedVersions = append(updatedVersions, version)
				}
				res.Spec.Versions = updatedVersions
			}
			var regErr error
			if _, regErr = destClnt.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), res, metav1.CreateOptions{}); regErr != nil && !errors.IsAlreadyExists(regErr) {
				log.MigrationLog(migration).Errorf("error registering crds v1 %s, %v", res.GetName(), regErr)
			}
			if regErr == nil {
				// wait for crd to be ready
				if err := k8sutils.ValidateCRDV1(destClnt, res.GetName()); err != nil {
					log.MigrationLog(migration).Errorf("Unable to validate crds v1 %v,%v", res.GetName(), err)
				}
			}
		}
	}

	// TODO: Create rancher projects if not present on the DR side.

	// First make sure all the namespaces are created on the
	// remote cluster
	for _, ns := range migrationNamespaces {
		namespace, err := core.Instance().GetNamespace(ns)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}

		// Don't create if the namespace already exists on the remote cluster
		_, err = remoteClient.adminClient.CoreV1().Namespaces().Get(context.TODO(), namespace.Name, metav1.GetOptions{})
		if err == nil {
			continue
		}

		annotations := m.getParsedAnnotations(namespace.Annotations, clusterPair)
		labels := m.getParsedLabels(namespace.Labels, clusterPair)
		_, err = remoteClient.adminClient.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        namespace.Name,
				Labels:      labels,
				Annotations: annotations,
			},
		}, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}

	var pvObjects, pvcObjects, updatedObjects []runtime.Unstructured
	pvMapping := make(map[string]v1.ObjectReference)
	// collect pv,pvc separately
	for _, o := range objects {
		objectType, err := meta.TypeAccessor(o)
		if err != nil {
			return err
		}
		switch objectType.GetKind() {
		case "PersistentVolume":
			pvObjects = append(pvObjects, o)
		case "PersistentVolumeClaim":
			pvcObjects = append(pvcObjects, o)
		default:
			updatedObjects = append(updatedObjects, o)
		}
	}

	// find out the csi PVs and if the volumeHandle does not match with pv Name , delete those.
	// https://portworx.atlassian.net/browse/PWX-30157
	pvToPVCMapping := getPVToPVCMappingFromPVCObjects(pvcObjects)
	var csiPVCAndPVObjects []runtime.Unstructured
	for _, obj := range pvObjects {
		var pv v1.PersistentVolume
		var err error
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &pv); err != nil {
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error unmarshalling pv resource: %v", err))
			continue
		}
		if pv.Spec.CSI != nil {
			respPV, err := remoteClient.adminClient.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
			if err != nil {
				logrus.Errorf("error getting pv %s: %v", pv.Name, err)
				continue
			}
			if respPV.Spec.CSI != nil && respPV.Spec.CSI.VolumeHandle != pv.Name {
				// Add the pvc object related to the PV for deleting
				if _, ok := pvToPVCMapping[pv.Name]; ok {
					csiPVCAndPVObjects = append(csiPVCAndPVObjects, pvToPVCMapping[pv.Name])
				}
				// Add the pv object to the list also for getting deleted
				csiPVCAndPVObjects = append(csiPVCAndPVObjects, obj)
			}
		}
	}
	if len(csiPVCAndPVObjects) > 0 {
		err = m.resourceCollector.DeleteResources(
			remoteClient.remoteAdminInterface,
			csiPVCAndPVObjects,
			nil)
		if err != nil {
			logrus.Errorf("error deleting csi pvcs and pvs: %v ", err)
			return err
		}
	}

	// create/update pv object with updated policy
	for _, obj := range pvObjects {
		var pv v1.PersistentVolume
		var err error
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &pv); err != nil {
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error unmarshalling pv resource: %v", err))
			continue
		}
		log.MigrationLog(migration).Infof("Applying %v %v", pv.Kind, pv.GetName())
		if pv.GetAnnotations() == nil {
			pv.Annotations = make(map[string]string)
		}
		pv.Annotations[StorkMigrationAnnotation] = "true"
		pv.Annotations[StorkMigrationName] = migration.GetName()
		pv.Annotations[StorkMigrationNamespace] = migration.GetNamespace()
		pv.Annotations[StorkMigrationTime] = time.Now().Format(nameTimeSuffixFormat)
		pv.Annotations = m.getParsedAnnotations(pv.Annotations, clusterPair)
		pv.Labels = m.getParsedLabels(pv.Labels, clusterPair)
		_, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Create(context.TODO(), &pv, metav1.CreateOptions{})
		if err != nil {
			if err != nil && errors.IsAlreadyExists(err) {
				var respPV *v1.PersistentVolume
				respPV, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
				if err == nil {
					// allow only annotation and reclaim policy update
					// TODO: idle way should be to use Patch
					if respPV.GetAnnotations() == nil {
						respPV.Annotations = make(map[string]string)
					}
					respPV.Annotations = pv.Annotations
					respPV.Spec.PersistentVolumeReclaimPolicy = pv.Spec.PersistentVolumeReclaimPolicy
					respPV.ResourceVersion = ""
					_, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Update(context.TODO(), respPV, metav1.UpdateOptions{})
				}
			}
		}
		if err != nil {
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", err))
			// fail migrations since we were not able to update pv reclaim object
			migration.Status.Stage = stork_api.MigrationStageFinal
			migration.Status.FinishTimestamp = metav1.Now()
			migration.Status.Status = stork_api.MigrationStatusFailed
			return m.updateMigrationCR(context.TODO(), migration)
		}
		m.updateResourceStatus(
			migration,
			obj,
			stork_api.MigrationStatusSuccessful,
			"Resource migrated successfully")
	}
	// apply pvc objects
	for _, obj := range pvcObjects {
		var pvc v1.PersistentVolumeClaim
		var err error
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &pvc); err != nil {
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", err))
			continue
		}
		objRef := v1.ObjectReference{
			Name:      pvc.GetName(),
			Namespace: pvc.GetNamespace(),
		}
		// skip if there is no change in pvc specs
		objHash, err := hashstructure.Hash(obj, &hashstructure.HashOptions{})
		if err != nil {
			msg := fmt.Errorf("unable to generate hash for an object %v %v, err: %v", pvc.Kind, pvc.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		}
		resp, err := remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
		if err == nil && resp != nil {
			if hash, ok := resp.Annotations[resourcecollector.StorkResourceHash]; ok {
				old, err := strconv.ParseUint(hash, 10, 64)
				if err == nil && old == objHash {
					log.MigrationLog(migration).Debugf("Skipping pvc update, no changes found since last migration %d/%d", old, objHash)
					m.updateResourceStatus(
						migration,
						obj,
						stork_api.MigrationStatusSuccessful,
						"Resource migrated successfully")
					objRef.UID = resp.GetUID()
					pvMapping[pvc.Spec.VolumeName] = objRef
					continue
				}
			}
		}
		pvMapping[pvc.Spec.VolumeName] = objRef
		deleteStart := metav1.Now()
		isDeleted := false
		err = remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			msg := fmt.Errorf("error deleting %v %v during migrate: %v", pvc.Kind, pvc.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		} else {
			for i := 0; i < deletedMaxRetries; i++ {
				obj, err := remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
				if err != nil && errors.IsNotFound(err) {
					isDeleted = true
					break
				}
				createTime := obj.GetCreationTimestamp()
				if deleteStart.Before(&createTime) {
					log.MigrationLog(migration).Warnf("Object[%v] got re-created after deletion. Not retrying deletion, deleteStart time:[%v], create time:[%v]",
						obj.GetName(), deleteStart, createTime)
					break
				}
				log.MigrationLog(migration).Infof("Object %v still present, retrying in %v", pvc.GetName(), deletedRetryInterval)
				time.Sleep(deletedRetryInterval)
			}
		}
		if !isDeleted {
			msg := fmt.Errorf("error in recreating pvc %s/%s during migration: %v", pvc.GetNamespace(), pvc.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		}
		if pvc.GetAnnotations() == nil {
			pvc.Annotations = make(map[string]string)
		}
		pvc.Annotations[StorkMigrationAnnotation] = "true"
		pvc.Annotations[StorkMigrationName] = migration.GetName()
		pvc.Annotations[StorkMigrationNamespace] = migration.GetNamespace()
		pvc.Annotations[StorkMigrationTime] = time.Now().Format(nameTimeSuffixFormat)
		pvc.Annotations[resourcecollector.StorkResourceHash] = strconv.FormatUint(objHash, 10)
		pvc.Annotations = m.getParsedAnnotations(pvc.Annotations, clusterPair)
		pvc.Labels = m.getParsedLabels(pvc.Labels, clusterPair)
		_, err = remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Create(context.TODO(), &pvc, metav1.CreateOptions{})
		if err != nil {
			msg := fmt.Errorf("error in recreating pvc %s/%s during migration: %v", pvc.GetNamespace(), pvc.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		}
		m.updateResourceStatus(
			migration,
			obj,
			stork_api.MigrationStatusSuccessful,
			"Resource migrated successfully")
	}
	// revert pv objects reclaim policy
	for _, obj := range pvObjects {
		var pv v1.PersistentVolume
		var pvc *v1.PersistentVolumeClaim
		var err error
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &pv); err != nil {
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", err))
			continue
		}
		if pvcObj, ok := pvMapping[pv.Name]; ok {
			pvc, err = remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvcObj.Namespace).Get(context.TODO(), pvcObj.Name, metav1.GetOptions{})
			if err != nil {
				msg := fmt.Errorf("error in retriving pvc info %s/%s during migration: %v", pvc.GetNamespace(), pvc.GetName(), err)
				m.updateResourceStatus(
					migration,
					obj,
					stork_api.MigrationStatusFailed,
					fmt.Sprintf("Error applying resource: %v", msg))
				continue
			}
			if pvcObj.UID != pvc.GetUID() {
				respPV, err := remoteClient.adminClient.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
				if err != nil {
					msg := fmt.Errorf("error in reading pv %s during migration: %v", pv.GetName(), err)
					m.updateResourceStatus(
						migration,
						obj,
						stork_api.MigrationStatusFailed,
						fmt.Sprintf("Error applying resource: %v", msg))
					continue
				}
				if respPV.Spec.ClaimRef == nil {
					respPV.Spec.ClaimRef = &v1.ObjectReference{
						Name:      pvc.GetName(),
						Namespace: pvc.GetNamespace(),
					}
				}
				respPV.Spec.ClaimRef.UID = pvc.GetUID()
				respPV.ResourceVersion = ""
				if _, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Update(context.TODO(), respPV, metav1.UpdateOptions{}); err != nil {
					msg := fmt.Errorf("error in updating pvc UID in pv %s during migration: %v", pv.GetName(), err)
					m.updateResourceStatus(
						migration,
						obj,
						stork_api.MigrationStatusFailed,
						fmt.Sprintf("Error applying resource: %v", msg))
					continue
				}
				// wait for pvc object to be in bound state
				isBound := false
				for i := 0; i < deletedMaxRetries*2; i++ {
					var resp *v1.PersistentVolumeClaim
					resp, err = remoteClient.adminClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
					if err != nil {
						msg := fmt.Errorf("error in retriving pvc %s/%s during migration: %v", pvc.GetNamespace(), pvc.GetName(), err)
						m.updateResourceStatus(
							migration,
							obj,
							stork_api.MigrationStatusFailed,
							fmt.Sprintf("Error applying resource: %v", msg))
						continue
					}
					if resp.Status.Phase == v1.ClaimBound {
						isBound = true
						break
					}
					log.MigrationLog(migration).Infof("PVC Object %s not bound yet, retrying in %v", pvc.GetName(), deletedRetryInterval)
					time.Sleep(boundRetryInterval)
				}
				if !isBound {
					msg := fmt.Errorf("pvc %s/%s is not in bound state ", pvc.GetNamespace(), pvc.GetName())
					m.updateResourceStatus(
						migration,
						obj,
						stork_api.MigrationStatusFailed,
						fmt.Sprintf("Error applying resource: %v", msg))
					continue
				}
			}
		}
		respPV, err := remoteClient.adminClient.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
		if err != nil {
			msg := fmt.Errorf("error in reading pv %s during migration: %v", pv.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		}
		// update pv's reclaim policy
		if pv.Annotations != nil && pv.Annotations[PVReclaimAnnotation] != "" {
			respPV.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimPolicy(pv.Annotations[PVReclaimAnnotation])
		}
		if migration.Spec.IncludeVolumes != nil && !*migration.Spec.IncludeVolumes {
			respPV.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimRetain
		}
		respPV.ResourceVersion = ""
		if _, err = remoteClient.adminClient.CoreV1().PersistentVolumes().Update(context.TODO(), respPV, metav1.UpdateOptions{}); err != nil {
			msg := fmt.Errorf("error in updating pv %s during migration: %v", pv.GetName(), err)
			m.updateResourceStatus(
				migration,
				obj,
				stork_api.MigrationStatusFailed,
				fmt.Sprintf("Error applying resource: %v", msg))
			continue
		}
		m.updateResourceStatus(
			migration,
			obj,
			stork_api.MigrationStatusSuccessful,
			"Resource migrated successfully")
	}

	appRegsStashMap := make(map[string]bool)
	if !*migration.Spec.StartApplications {
		appRegsStashMap = getAppRegsStashMap(*crdList)
	}
	// apply remaining objects
	worker := func(objectChan <-chan runtime.Unstructured, errorChan chan<- error, appRegsStashMap map[string]bool) {
		var stashCMError bool
		for o := range objectChan {
			metadata, err := meta.Accessor(o)
			if err != nil {
				errorChan <- err
			}
			objectType, err := meta.TypeAccessor(o)
			if err != nil {
				errorChan <- err
			}

			unstructured, ok := o.(*unstructured.Unstructured)
			if !ok {
				errorChan <- fmt.Errorf("unable to cast object to unstructured: %v", o)
			}

			// set migration annotations
			migrAnnot := metadata.GetAnnotations()
			if migrAnnot == nil {
				migrAnnot = make(map[string]string)
			}
			migrAnnot[StorkMigrationAnnotation] = "true"
			migrAnnot[StorkMigrationName] = migration.GetName()
			migrAnnot[StorkMigrationNamespace] = migration.GetNamespace()
			migrAnnot[StorkMigrationTime] = time.Now().Format(nameTimeSuffixFormat)
			migrAnnot = m.getParsedAnnotations(migrAnnot, clusterPair)

			objHash, err := hashstructure.Hash(o, &hashstructure.HashOptions{})
			if err != nil {
				log.MigrationLog(migration).Warnf("unable to generate hash for an object %v %v, err: %v", objectType.GetKind(), metadata.GetName(), err)
			}
			migrAnnot[resourcecollector.StorkResourceHash] = strconv.FormatUint(objHash, 10)
			unstructured.SetAnnotations(migrAnnot)

			// parse and set labels on all objects
			currentLabels := metadata.GetLabels()
			newLabels := m.getParsedLabels(currentLabels, clusterPair)
			if len(newLabels) > 0 {
				unstructured.SetLabels(newLabels)
			}

			resource := &metav1.APIResource{
				Name:       ruleset.Pluralize(strings.ToLower(objectType.GetKind())),
				Namespaced: len(metadata.GetNamespace()) > 0,
			}
			var dynamicClient dynamic.ResourceInterface

			retries := 0

			gvk := o.GetObjectKind().GroupVersionKind()
			keyName := getAppRegsStashMapKeyName(gvk.Group, gvk.Kind, gvk.Version)
			stashCREnabled := appRegsStashMap[keyName]
			if !stashCREnabled {
				if resource.Namespaced {
					dynamicClient = remoteClient.remoteInterface.Resource(
						o.GetObjectKind().GroupVersionKind().GroupVersion().WithResource(resource.Name)).Namespace(metadata.GetNamespace())
				} else {
					dynamicClient = remoteClient.remoteAdminInterface.Resource(
						o.GetObjectKind().GroupVersionKind().GroupVersion().WithResource(resource.Name))
				}
			} else {
				log.MigrationLog(migration).Infof("Getting configmap details for stashing resource %s/%s/%s", unstructured.GetAPIVersion(), unstructured.GetKind(), unstructured.GetName())
				unstructured, err = getStashedConfigMap(unstructured, objHash)
				if err != nil {
					log.MigrationLog(migration).Warnf("unable to get stashed configmap content for object %s/%s/%s, error: %v", unstructured.GetAPIVersion(), unstructured.GetKind(), unstructured.GetName(), err)
					stashCMError = true
				}
				if !stashCMError {
					resource = &metav1.APIResource{
						Name:       "configmaps",
						Namespaced: len(metadata.GetNamespace()) > 0,
					}
					dynamicClient = remoteClient.remoteInterface.Resource(
						v1.SchemeGroupVersion.WithResource("configmaps")).Namespace(unstructured.GetNamespace())
				}
			}

			if !stashCMError {
				log.MigrationLog(migration).Infof("Applying %v %v", objectType.GetKind(), metadata.GetName())
				for {
					_, err = dynamicClient.Create(context.TODO(), unstructured, metav1.CreateOptions{})
					if err != nil && (errors.IsAlreadyExists(err) || strings.Contains(err.Error(), portallocator.ErrAllocated.Error())) {
						switch objectType.GetKind() {
						case "ServiceAccount":
							err = m.checkAndUpdateDefaultSA(migration, o)
						case "Service":
							var skipUpdate bool
							skipUpdate, err = m.isServiceUpdated(migration, o, objHash)
							if err == nil && skipUpdate && len(migration.Spec.TransformSpecs) == 0 {
								break
							}
							fallthrough
						default:
							// Check the objecthash before deleting
							equalHash := isObjectHashEqual(dynamicClient, unstructured, objHash)
							if equalHash {
								log.MigrationLog(migration).Infof("skipping update for resource %s/%s, no changes found since last migration", objectType.GetKind(), metadata.GetName())
								// resetting the error as the resource status needs to be correctly updated in migration CR.
								err = nil
							} else {
								// Delete the resource if it already exists on the destination
								// cluster and try creating again
								deleteStart := metav1.Now()
								err = dynamicClient.Delete(context.TODO(), unstructured.GetName(), metav1.DeleteOptions{})
								if err != nil && !errors.IsNotFound(err) {
									log.MigrationLog(migration).Errorf("Error deleting %v %v during migrate: %v", objectType.GetKind(), metadata.GetName(), err)
								} else {
									// wait for resources to get deleted
									// 2 mins
									for i := 0; i < deletedMaxRetries; i++ {
										obj, err := dynamicClient.Get(context.TODO(), unstructured.GetName(), metav1.GetOptions{})
										if err != nil && errors.IsNotFound(err) {
											break
										}
										createTime := obj.GetCreationTimestamp()
										if deleteStart.Before(&createTime) {
											log.MigrationLog(migration).Warnf("Object[%v] got re-created after deletion. So, Ignore wait. deleteStart time:[%v], create time:[%v]",
												obj.GetName(), deleteStart, createTime)
											break
										}
										if obj.GetFinalizers() != nil {
											obj.SetFinalizers(nil)
											_, err = dynamicClient.Update(context.TODO(), obj, metav1.UpdateOptions{})
											if err != nil {
												log.MigrationLog(migration).Warnf("unable to delete finalizer for object %v", metadata.GetName())
											}
										}
										log.MigrationLog(migration).Warnf("Object %v still present, retrying in %v", metadata.GetName(), deletedRetryInterval)
										time.Sleep(deletedRetryInterval)
									}
									_, err = dynamicClient.Create(context.TODO(), unstructured, metav1.CreateOptions{})
								}
							}
						}
					}
					// Retry a few times for Unauthorized errors
					if err != nil && errors.IsUnauthorized(err) && retries < maxApplyRetries {
						retries++
						continue
					}
					break
				}
			}

			if err != nil {
				m.updateResourceStatus(
					migration,
					o,
					stork_api.MigrationStatusFailed,
					fmt.Sprintf("Error applying resource: %v", err))
			} else {
				m.updateResourceStatus(
					migration,
					o,
					stork_api.MigrationStatusSuccessful,
					"Resource migrated successfully")
			}
			errorChan <- nil
		}
	}
	return m.parallelWorker(worker, updatedObjects, appRegsStashMap, true)
}

func (m *MigrationController) parallelWorker(
	worker func(<-chan runtime.Unstructured, chan<- error, map[string]bool),
	objects []runtime.Unstructured,
	appRegsStashMap map[string]bool,
	shuffle bool,
) error {
	numObjects := len(objects)
	objectChan := make(chan runtime.Unstructured)
	errorChan := make(chan error)

	if shuffle {
		// Shuffle Object order before applying so we can get parallelism between resource types
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(objects), func(i, j int) { objects[i], objects[j] = objects[j], objects[i] })
	}

	logrus.Infof("Updating %v objects with %v parallel workers", numObjects, m.migrationMaxThreads)
	for w := 0; w < m.migrationMaxThreads; w++ {
		go worker(objectChan, errorChan, appRegsStashMap)
	}

	go func() {
		for _, o := range objects {
			objectChan <- o
		}
	}()

	for result := 0; result < numObjects; result++ {
		workerErr := <-errorChan
		if workerErr != nil {
			close(objectChan)
			return workerErr
		}
	}
	close(objectChan)
	return nil
}

func (m *MigrationController) getMigrationSummary(migration *stork_api.Migration) *stork_api.MigrationSummary {
	migrationSummary := &stork_api.MigrationSummary{}
	var totalBytes uint64
	if migration.Spec.IncludeVolumes == nil || *migration.Spec.IncludeVolumes {
		totalVolumes := uint64(len(migration.Status.Volumes))
		doneVolumes := uint64(0)
		for _, volume := range migration.Status.Volumes {
			if volume.Status == stork_api.MigrationStatusSuccessful {
				doneVolumes++
				totalBytes = totalBytes + volume.BytesTotal
			}
		}
		if totalVolumes > 0 {
			migrationSummary.TotalNumberOfVolumes = totalVolumes
			migrationSummary.NumberOfMigratedVolumes = doneVolumes
		}
		elapsedTimeVolume := "NA"
		if !migration.CreationTimestamp.IsZero() {
			if migration.Status.Stage == stork_api.MigrationStageApplications || migration.Status.Stage == stork_api.MigrationStageFinal {
				if !migration.Status.VolumeMigrationFinishTimestamp.IsZero() {
					elapsedTimeVolume = migration.Status.VolumeMigrationFinishTimestamp.Sub(migration.CreationTimestamp.Time).String()
				}
			} else { // Volume migration hasn't finished, use current total time
				elapsedTimeVolume = time.Since(migration.CreationTimestamp.Time).String()
			}
		}
		migrationSummary.ElapsedTimeForVolumeMigration = elapsedTimeVolume
	}

	totalResources := uint64(len(migration.Status.Resources))
	doneResources := uint64(0)
	for _, resource := range migration.Status.Resources {
		if resource.Status == stork_api.MigrationStatusSuccessful {
			doneResources++
		}
	}
	if totalResources > 0 {
		migrationSummary.TotalNumberOfResources = totalResources
		migrationSummary.NumberOfMigratedResources = doneResources
	}
	elapsedTimeResources := "NA"
	if !migration.Status.VolumeMigrationFinishTimestamp.IsZero() {
		if migration.Status.Stage == stork_api.MigrationStageFinal {
			if !migration.Status.ResourceMigrationFinishTimestamp.IsZero() {
				elapsedTimeResources = migration.Status.ResourceMigrationFinishTimestamp.Sub(migration.Status.VolumeMigrationFinishTimestamp.Time).String()
			}
		} else {
			elapsedTimeResources = time.Since(migration.Status.VolumeMigrationFinishTimestamp.Time).String()
		}
	}
	migrationSummary.ElapsedTimeForResourceMigration = elapsedTimeResources

	migrationSummary.TotalBytesMigrated = totalBytes
	return migrationSummary
}

func (m *MigrationController) cleanup(migration *stork_api.Migration) error {
	if migration.Status.Stage != stork_api.MigrationStageFinal {
		return m.volDriver.CancelMigration(migration)
	}
	return nil
}

func (m *MigrationController) createCRD() error {
	resource := apiextensions.CustomResource{
		Name:    stork_api.MigrationResourceName,
		Plural:  stork_api.MigrationResourcePlural,
		Group:   stork_api.SchemeGroupVersion.Group,
		Version: stork_api.SchemeGroupVersion.Version,
		Scope:   apiextensionsv1beta1.NamespaceScoped,
		Kind:    reflect.TypeOf(stork_api.Migration{}).Name(),
	}
	ok, err := version.RequiresV1Registration()
	if err != nil {
		return err
	}
	if ok {
		err := k8sutils.CreateCRDV1(resource)
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		return apiextensions.Instance().ValidateCRD(resource.Plural+"."+resource.Group, validateCRDTimeout, validateCRDInterval)
	}
	err = apiextensions.Instance().CreateCRDV1beta1(resource)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return apiextensions.Instance().ValidateCRDV1beta1(resource, validateCRDTimeout, validateCRDInterval)
}

func (m *MigrationController) getVolumeOnlyMigrationResources(
	migration *stork_api.Migration,
	migrationNamespaces []string,
	resourceCollectorOpts resourcecollector.Options,
) ([]runtime.Unstructured, []v1.PersistentVolumeClaim, error) {
	var resources []runtime.Unstructured
	var pvcWithOwnerRef []v1.PersistentVolumeClaim
	// add pv objects
	resource := metav1.APIResource{
		Name:       "persistentvolumes",
		Kind:       "PersistentVolume",
		Version:    "v1",
		Namespaced: false,
	}
	objects, _, err := m.resourceCollector.GetResourcesForType(
		resource,
		nil,
		migrationNamespaces,
		migration.Spec.Selectors,
		migration.Spec.ExcludeSelectors,
		nil,
		false,
		resourceCollectorOpts,
	)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error getting pv resource: %v", err))
		log.MigrationLog(migration).Errorf("Error getting pv resources: %v", err)
		return resources, nil, err
	}
	resources = append(resources, objects.Items...)
	// add pvcs to resource list
	resource = metav1.APIResource{
		Name:       "persistentvolumeclaims",
		Kind:       "PersistentVolumeClaim",
		Version:    "v1",
		Namespaced: true,
	}
	objects, pvcWithOwnerRef, err = m.resourceCollector.GetResourcesForType(
		resource,
		nil,
		migrationNamespaces,
		migration.Spec.Selectors,
		migration.Spec.ExcludeSelectors,
		nil,
		false,
		resourceCollectorOpts,
	)
	if err != nil {
		m.recorder.Event(migration,
			v1.EventTypeWarning,
			string(stork_api.MigrationStatusFailed),
			fmt.Sprintf("Error getting pv resource: %v", err))
		log.MigrationLog(migration).Errorf("Error getting pv resources: %v", err)
		return resources, nil, err
	}
	resources = append(resources, objects.Items...)
	return resources, pvcWithOwnerRef, nil
}

func getPVToPVCMappingFromPVCObjects(pvcObjects []runtime.Unstructured) map[string]runtime.Unstructured {
	pvToPVCMapping := make(map[string]runtime.Unstructured)
	for _, obj := range pvcObjects {
		var pvc v1.PersistentVolumeClaim
		var err error
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &pvc); err != nil {
			logrus.Errorf("Error unmarshalling pvc resource: %v", err)
			continue
		}
		if len(pvc.Spec.VolumeName) > 0 {
			pvToPVCMapping[pvc.Spec.VolumeName] = obj
		}
	}
	return pvToPVCMapping
}

func getAppRegsStashMap(crdList stork_api.ApplicationRegistrationList) map[string]bool {
	appRegsStashMap := make(map[string]bool)
	for _, crd := range crdList.Items {
		for _, v := range crd.Resources {
			appKey := getAppRegsStashMapKeyName(v.Group, v.Kind, v.Version)
			appRegsStashMap[appKey] = v.StashStrategy.StashCR
		}
	}
	return appRegsStashMap
}

func getAppRegsStashMapKeyName(group string, version string, kind string) string {
	return fmt.Sprintf("%v-%v-%v", group, kind, version)
}

func getStashedConfigMap(obj *unstructured.Unstructured, inputObjectHash uint64) (*unstructured.Unstructured, error) {
	configMap := &unstructured.Unstructured{}
	jsonData, err := obj.MarshalJSON()
	if err != nil {
		return configMap, err
	}
	// obj uid is not available and appending random uid will cause the next migration's get/delete of the configmap fail
	// hence adding kind-name for cm name.
	cmName := utils.GetStashedConfigMapName(strings.ToLower(obj.GetKind()), strings.ToLower(obj.GroupVersionKind().Group), obj.GetName())

	ownedPVCs := make(map[string][]metav1.OwnerReference)
	ownedPVCsEncoded, err := json.Marshal(ownedPVCs)
	if err != nil {
		return configMap, err
	}
	configMapSpec := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: obj.GetNamespace(),
			Labels: map[string]string{
				StashCRLabel:    "true",
				"resource-kind": obj.GetKind(),
			},
			Annotations: map[string]string{
				skipResourceAnnotation: "true",
				storkCreatedAnnotation: "true",
				// Using the objecthash of the CR which is getting put so that the objecthash check matches if there is no changes to the CR.
				// Recalculating the hash on the configmap content will always change as the CR object has an annotation for the migration name.
				resourcecollector.StorkResourceHash: strconv.FormatUint(inputObjectHash, 10),
			},
		},
		Data: map[string]string{
			StashedCMCRKey:       string(jsonData),
			StashedCMOwnedPVCKey: string(ownedPVCsEncoded),
			StashedCMCRNameKey:   obj.GetName(),
		},
	}

	cm, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMapSpec)
	if err != nil {
		return nil, err
	}
	configMap = &unstructured.Unstructured{Object: cm}
	return configMap, nil
}

func updateStashedCMWithPVCInfo(remoteClient *RemoteClient, configMapName string, namespace string, pvcName string, ownerReference metav1.OwnerReference) error {
	cm, err := remoteClient.adminClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Errorf("error getting stashed configmap %s in namespace %s", configMapName, namespace)
		}
		return err
	}
	existingValue := cm.Data[StashedCMOwnedPVCKey]
	nestedPVCOwnerReferenceMap := make(map[string]metav1.OwnerReference)
	err = json.Unmarshal([]byte(existingValue), &nestedPVCOwnerReferenceMap)
	if err != nil {
		return err
	}
	nestedPVCOwnerReferenceMap[pvcName] = ownerReference
	// Convert the nested map back to a string
	newValue, err := json.Marshal(nestedPVCOwnerReferenceMap)
	if err != nil {
		return err
	}
	cm.Data[StashedCMOwnedPVCKey] = string(newValue)

	// Update the ConfigMap
	_, err = remoteClient.adminClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func isObjectHashEqual(dynamicClient dynamic.ResourceInterface, u *unstructured.Unstructured, inputHashValue uint64) bool {
	unstructuredDestination, err := dynamicClient.Get(context.TODO(), u.GetName(), metav1.GetOptions{})
	if err != nil {
		return false
	}
	content := unstructuredDestination.UnstructuredContent()
	annotations, found, err := unstructured.NestedStringMap(content, "metadata", "annotations")
	if err != nil {
		return false
	}
	if found {
		if resHash, ok := annotations[resourcecollector.StorkResourceHash]; ok {
			existingHashValue, err := strconv.ParseUint(resHash, 10, 64)
			if err != nil {
				return false
			}
			if existingHashValue == inputHashValue {
				return true
			}
		}
	}
	return false
}

// getRelatedCRDListWRTGroupAndCategories takes AppRegs as input and
// finds out all the CRDs that need to be collected.
// The CRD list should contain
//
//	a. CRD matching with the required groups.
//	b. CRDs which are related with the CRDs from above list (#a)
func getRelatedCRDListWRTGroupAndCategories(client *apiextensionsclient.Clientset, ruleset *inflect.Ruleset, crdList *stork_api.ApplicationRegistrationList, resGroups map[string]string) []*stork_api.ApplicationRegistration {
	filteredCRDList := make([]*stork_api.ApplicationRegistration, 0)
	reqCategoriesMap := make(map[string]bool)
	// find all related categories from the CRDs having same group
	for _, crd := range crdList.Items {
		for _, v := range crd.Resources {
			if _, ok := resGroups[v.Group]; !ok {
				continue
			}
			crdName := ruleset.Pluralize(strings.ToLower(v.Kind)) + "." + v.Group
			crdvbeta1, err := client.ApiextensionsV1beta1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
			if err == nil {
				for _, category := range crdvbeta1.Spec.Names.Categories {
					if !slice.ContainsString(catergoriesExcludeList, category, strings.ToLower) {
						reqCategoriesMap[category] = true
					}
				}
				continue
			}
			crdv1, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
			if err == nil {
				for _, category := range crdv1.Spec.Names.Categories {
					if !slice.ContainsString(catergoriesExcludeList, category, strings.ToLower) {
						reqCategoriesMap[category] = true
					}
				}
				continue
			}
		}
	}
	logrus.Infof("crd categories to include are: %+v", reqCategoriesMap)

	// find all crds matching the group and having related categories
	for _, tempCRD := range crdList.Items {
		crd := tempCRD
		for _, v := range crd.Resources {
			if _, ok := resGroups[v.Group]; ok {
				filteredCRDList = append(filteredCRDList, &crd)
				continue
			}
			if len(reqCategoriesMap) > 0 {
				crdName := ruleset.Pluralize(strings.ToLower(v.Kind)) + "." + v.Group
				crdvbeta1, err := client.ApiextensionsV1beta1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
				if err == nil {
					for _, category := range crdvbeta1.Spec.Names.Categories {
						if _, ok := reqCategoriesMap[category]; ok {
							filteredCRDList = append(filteredCRDList, &crd)
							continue
						}
					}
					continue
				}
				crdv1, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
				if err == nil {
					for _, category := range crdv1.Spec.Names.Categories {
						if _, ok := reqCategoriesMap[category]; ok {
							filteredCRDList = append(filteredCRDList, &crd)
							continue
						}
					}
					continue
				}
			}
		}
	}

	return filteredCRDList
}

func (m *MigrationController) getResources(
	namespaces []string,
	migration *stork_api.Migration,
	labelSelectors map[string]string,
	excludeSelectors map[string]string,
	resourceCollectorOpts resourcecollector.Options,
	remote bool,
) ([]runtime.Unstructured, []v1.PersistentVolumeClaim, error) {

	var objects []runtime.Unstructured
	var pvcs []v1.PersistentVolumeClaim
	var err error

	rc := m.resourceCollector
	if remote {
		rc = resourcecollector.ResourceCollector{
			Driver: m.volDriver,
		}
		remoteConfig, err := getClusterPairSchedulerConfig(migration.Spec.ClusterPair, migration.Namespace)
		if err != nil {
			return objects, pvcs, err
		}

		log.MigrationLog(migration).Infof("Setting context for getting resources in remote cluster")
		// use seperate resource collector for collecting resources
		// from destination cluster
		err = rc.Init(remoteConfig)
		if err != nil {
			log.MigrationLog(migration).Errorf("Error initializing resource collector: %v", err)
			return objects, pvcs, err
		}
	}

	if len(migration.Spec.ExcludeResourceTypes) > 0 {
		objects, pvcs, err = rc.GetResourcesExcludingTypes(
			namespaces,
			migration.Spec.ExcludeResourceTypes,
			labelSelectors,
			excludeSelectors,
			nil,
			migration.Spec.IncludeOptionalResourceTypes,
			false,
			resourceCollectorOpts,
		)
	} else {
		objects, pvcs, err = rc.GetResources(
			namespaces,
			labelSelectors,
			excludeSelectors,
			nil,
			migration.Spec.IncludeOptionalResourceTypes,
			false,
			resourceCollectorOpts,
		)
	}

	return objects, pvcs, err
}
