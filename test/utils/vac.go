package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type VACOptions struct {
	Namespace        string
	Image            string
	Username         string
	Password         string
	StorageClass     string
	StandardClass    string
	PerformanceClass string
	Size             string
	Timeout          time.Duration
}

func VACOptionsFromEnvironment(namespace, image, username, password string) (VACOptions, error) {
	options := VACOptions{
		Namespace: namespace, Image: image, Username: username, Password: password,
		StorageClass: os.Getenv("E2E_VAC_STORAGE_CLASS"), StandardClass: os.Getenv("E2E_VAC_STANDARD_CLASS"),
		PerformanceClass: os.Getenv("E2E_VAC_PERFORMANCE_CLASS"), Size: "20Gi", Timeout: 15 * time.Minute,
	}
	if size := os.Getenv("E2E_VAC_SIZE"); size != "" {
		options.Size = size
	}
	if timeout := os.Getenv("E2E_VAC_TIMEOUT"); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil || parsed <= 0 {
			return options, fmt.Errorf("E2E_VAC_TIMEOUT must be a positive duration")
		}
		options.Timeout = parsed
	}
	if options.StorageClass == "" || options.StandardClass == "" || options.PerformanceClass == "" || options.Image == "" {
		return options, fmt.Errorf("VAC e2e requires E2E_MARKLOGIC_IMAGE_VERSION, E2E_VAC_STORAGE_CLASS, E2E_VAC_STANDARD_CLASS and E2E_VAC_PERFORMANCE_CLASS")
	}
	if options.StandardClass == options.PerformanceClass {
		return options, fmt.Errorf("VAC standard and performance classes must be different")
	}
	size, err := resource.ParseQuantity(options.Size)
	if err != nil || size.Sign() <= 0 {
		return options, fmt.Errorf("E2E_VAC_SIZE must be a positive storage quantity")
	}
	return options, nil
}

type vacFixture struct {
	cluster string
	groups  []string
	uid     types.UID
}

type vacRun struct {
	t           *testing.T
	ctx         context.Context
	options     VACOptions
	client      client.Client
	config      *rest.Config
	prefix      string
	version     string
	fixtures    []vacFixture
	claims      map[string]types.UID
	standard    *unstructured.Unstructured
	performance *unstructured.Unstructured
}

type vacSnapshot struct {
	set     *appsv1.StatefulSet
	pod     *corev1.Pod
	claims  map[string]*corev1.PersistentVolumeClaim
	volumes map[string]*corev1.PersistentVolume
}

func newVACRun(ctx context.Context, t *testing.T, config *envconf.Config, options VACOptions) *vacRun {
	t.Helper()
	testScheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, storagev1.AddToScheme, marklogicv1.AddToScheme} {
		if err := register(testScheme); err != nil {
			t.Fatal(err)
		}
	}
	apiConfig := rest.CopyConfig(config.Client().RESTConfig())
	apiConfig.Timeout = 30 * time.Second
	apiClient, err := client.New(apiConfig, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	run := &vacRun{t: t, ctx: ctx, options: options, client: apiClient, config: apiConfig,
		prefix: "vac-" + rand.String(8), claims: map[string]types.UID{}}
	run.preflight()
	return run
}

func (run *vacRun) preflight() {
	discoveryConfig := rest.CopyConfig(run.config)
	discoveryConfig.Timeout = 15 * time.Second
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(discoveryConfig)
	run.must(err)
	for _, version := range []string{"v1", "v1beta1"} {
		resources, err := discoveryClient.ServerResourcesForGroupVersion("storage.k8s.io/" + version)
		if apierrors.IsNotFound(err) {
			continue
		}
		run.must(err)
		for _, discovered := range resources.APIResources {
			if discovered.Name == "volumeattributesclasses" {
				run.version = version
				break
			}
		}
		if run.version != "" {
			break
		}
	}
	if run.version == "" {
		run.t.Fatal("VAC e2e was enabled but neither the GA nor beta VAC API is served")
	}
	storageClass := &storagev1.StorageClass{}
	run.must(run.client.Get(run.ctx, client.ObjectKey{Name: run.options.StorageClass}, storageClass))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.options.Namespace}}
	if err := run.client.Get(run.ctx, client.ObjectKey{Name: namespace.Name}, namespace); apierrors.IsNotFound(err) {
		run.must(run.client.Create(run.ctx, namespace))
	} else {
		run.must(err)
	}
	run.standard = run.profile(run.options.StandardClass)
	run.performance = run.profile(run.options.PerformanceClass)
	for _, profile := range []*unstructured.Unstructured{run.standard, run.performance} {
		if profile.GetDeletionTimestamp() != nil {
			run.t.Fatalf("VAC %s is terminating", profile.GetName())
		}
		driver, _, err := unstructured.NestedString(profile.Object, "driverName")
		run.must(err)
		if driver != storageClass.Provisioner {
			run.t.Fatalf("VAC %s driver %s does not match StorageClass provisioner %s", profile.GetName(), driver, storageClass.Provisioner)
		}
	}
	if reflect.DeepEqual(run.standard.Object["parameters"], run.performance.Object["parameters"]) {
		run.t.Fatal("standard and performance VACs must have different parameters")
	}
	if storageClass.Provisioner == "ebs.csi.aws.com" {
		_, err := exec.LookPath("aws")
		run.must(err)
		if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			run.t.Fatal("EBS verification requires AWS_REGION or AWS_DEFAULT_REGION")
		}
		for _, profile := range []*unstructured.Unstructured{run.standard, run.performance} {
			parameters, _, err := unstructured.NestedStringMap(profile.Object, "parameters")
			run.must(err)
			if parameters["type"] != "gp3" || parameters["iops"] == "" || parameters["throughput"] == "" {
				run.t.Fatal("EBS core fixtures require complete gp3 VACs with explicit type, iops and throughput for provider verification")
			}
		}
	}
	run.t.Logf("VAC lane: namespace=%s API=%s StorageClass=%s driver=%s", run.options.Namespace, run.version, storageClass.Name, storageClass.Provisioner)
}

func (run *vacRun) must(err error) {
	run.t.Helper()
	if err != nil {
		run.t.Fatal(err)
	}
}

func (run *vacRun) key(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: run.options.Namespace, Name: name}
}

func (run *vacRun) eventually(description string, check func() error) {
	run.t.Helper()
	var lastErr error
	err := wait.PollUntilContextTimeout(run.ctx, 5*time.Second, run.options.Timeout, true, func(context.Context) (bool, error) {
		lastErr = check()
		return lastErr == nil, nil
	})
	if err != nil {
		run.t.Fatalf("%s: %v (last observation: %v)", description, err, lastErr)
	}
}

func (run *vacRun) profile(name string) *unstructured.Unstructured {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage.k8s.io", Version: run.version, Kind: "VolumeAttributesClass"})
	run.must(run.client.Get(run.ctx, client.ObjectKey{Name: name}, profile))
	return profile
}

func (run *vacRun) persistence(className string) *marklogicv1.Persistence {
	persistence := &marklogicv1.Persistence{Enabled: true, Size: run.options.Size, StorageClassName: run.options.StorageClass,
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Annotations: map[string]string{"vac-e2e-run": run.prefix}}
	if className != "" {
		persistence.VolumeAttributesClassName = &className
	}
	return persistence
}

func (run *vacRun) additional(className string) *[]corev1.PersistentVolumeClaim {
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "audit", Labels: map[string]string{"vac-e2e": run.prefix}, Annotations: map[string]string{"vac-e2e-metadata": "preserved", "vac-e2e-run": run.prefix}},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &run.options.StorageClass, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(run.options.Size)}}},
	}
	if className != "" {
		claim.Spec.VolumeAttributesClassName = &className
	}
	return &[]corev1.PersistentVolumeClaim{claim}
}

func (run *vacRun) cluster(suffix, primary, additional string, configure func(*marklogicv1.MarklogicCluster)) vacFixture {
	name := run.prefix + "-" + suffix
	replicas := int32(1)
	cluster := &marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.options.Namespace},
		Spec: marklogicv1.MarklogicClusterSpec{Image: run.options.Image,
			Auth:           &marklogicv1.AdminAuth{AdminUsername: &run.options.Username, AdminPassword: &run.options.Password},
			UpdateStrategy: appsv1.OnDeleteStatefulSetStrategyType, Persistence: run.persistence(primary),
			AdditionalVolumeClaimTemplates: run.additional(additional), AdditionalVolumeMounts: &[]corev1.VolumeMount{{Name: "audit", MountPath: "/var/opt/MarkLogic-extra"}},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{{Name: name + "-node", Replicas: &replicas, IsBootstrap: true}}}}
	if configure != nil {
		configure(cluster)
	}
	run.must(run.client.Create(run.ctx, cluster))
	fixture := vacFixture{cluster: name, uid: cluster.UID}
	for _, group := range cluster.Spec.MarkLogicGroups {
		fixture.groups = append(fixture.groups, group.Name)
	}
	run.fixtures = append(run.fixtures, fixture)
	return fixture
}

func (run *vacRun) group(name string) *marklogicv1.MarklogicGroup {
	group := &marklogicv1.MarklogicGroup{}
	run.must(run.client.Get(run.ctx, run.key(name), group))
	return group
}

func (run *vacRun) patch(object client.Object, fields map[string]interface{}) {
	run.t.Helper()
	data, err := json.Marshal(fields)
	run.must(err)
	run.must(run.client.Patch(run.ctx, object, client.RawPatch(types.MergePatchType, data)))
}

func (run *vacRun) patchCluster(fixture vacFixture, fields map[string]interface{}) {
	run.patch(&marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: fixture.cluster, Namespace: run.options.Namespace}}, map[string]interface{}{"spec": fields})
}

func (run *vacRun) claim(group, template string, ordinal int32) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{}
	run.must(run.client.Get(run.ctx, run.key(fmt.Sprintf("%s-%s-%d", template, group, ordinal)), claim))
	run.claims[claim.Name] = claim.UID
	return claim
}

func (run *vacRun) ready(group string, replicas int32) {
	run.eventually("workload ready: "+group, func() error {
		set := &appsv1.StatefulSet{}
		if err := run.client.Get(run.ctx, run.key(group), set); err != nil {
			return err
		}
		if set.Spec.Replicas == nil || *set.Spec.Replicas != replicas || set.Status.ObservedGeneration < set.Generation || set.Status.ReadyReplicas != replicas {
			return fmt.Errorf("StatefulSet desired=%v ready=%d, expected=%d", set.Spec.Replicas, set.Status.ReadyReplicas, replicas)
		}
		for ordinal := int32(0); ordinal < replicas; ordinal++ {
			pod := &corev1.Pod{}
			if err := run.client.Get(run.ctx, run.key(fmt.Sprintf("%s-%d", group, ordinal)), pod); err != nil {
				return err
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
			}
			if !ready || pod.DeletionTimestamp != nil {
				return fmt.Errorf("Pod %s is not ready", pod.Name)
			}
		}
		return nil
	})
}

func vacProgressCurrent(generation int64, storage *marklogicv1.StorageStatus, conditions []metav1.Condition) error {
	condition := meta.FindStatusCondition(conditions, "StorageAttributesSynced")
	if storage == nil || storage.VolumeAttributesClass == nil || storage.VolumeAttributesClass.ObservedGeneration != generation ||
		storage.VolumeAttributesClass.State != marklogicv1.VolumeAttributesClassCompleted || condition == nil ||
		condition.ObservedGeneration != generation || condition.Status != metav1.ConditionTrue {
		return fmt.Errorf("VAC completion is missing, stale, or not successful for generation %d", generation)
	}
	return nil
}

func (run *vacRun) complete(fixture vacFixture, expected map[string][2]string, replicas int32) {
	run.eventually("current native and CR VAC completion", func() error {
		generations := map[string]int64{}
		for groupName, classes := range expected {
			group := &marklogicv1.MarklogicGroup{}
			if err := run.client.Get(run.ctx, run.key(groupName), group); err != nil {
				return err
			}
			if group.Spec.Persistence == nil || classValue(group.Spec.Persistence.VolumeAttributesClassName) != classes[0] || group.Spec.AdditionalVolumeClaimTemplates == nil || len(*group.Spec.AdditionalVolumeClaimTemplates) != 1 ||
				classValue((*group.Spec.AdditionalVolumeClaimTemplates)[0].Spec.VolumeAttributesClassName) != classes[1] {
				return fmt.Errorf("child %s has not received desired selections", groupName)
			}
			if err := vacProgressCurrent(group.Generation, group.Status.Storage, group.Status.Conditions); err != nil {
				return fmt.Errorf("group %s: %w", groupName, err)
			}
			generations[groupName] = group.Generation
			managedTemplates := 0
			for _, className := range classes {
				if className != "" {
					managedTemplates++
				}
			}
			if len(group.Status.Storage.VolumeAttributesClass.PVCs) != managedTemplates*int(replicas) {
				return fmt.Errorf("group %s has an incomplete active-PVC status inventory", groupName)
			}
			for template, className := range map[string]string{"datadir": classes[0], "audit": classes[1]} {
				for ordinal := int32(0); ordinal < replicas; ordinal++ {
					claim := &corev1.PersistentVolumeClaim{}
					if err := run.client.Get(run.ctx, run.key(fmt.Sprintf("%s-%s-%d", template, groupName, ordinal)), claim); err != nil {
						return err
					}
					run.claims[claim.Name] = claim.UID
					if claim.Status.Phase != corev1.ClaimBound || classValue(claim.Spec.VolumeAttributesClassName) != className ||
						classValue(claim.Status.CurrentVolumeAttributesClassName) != className {
						return fmt.Errorf("PVC %s requested=%s applied=%s expected=%s", claim.Name, classValue(claim.Spec.VolumeAttributesClassName), classValue(claim.Status.CurrentVolumeAttributesClassName), className)
					}
					if claim.Status.ModifyVolumeStatus != nil && (claim.Status.ModifyVolumeStatus.Status == corev1.PersistentVolumeClaimModifyVolumePending || claim.Status.ModifyVolumeStatus.Status == corev1.PersistentVolumeClaimModifyVolumeInProgress) {
						return fmt.Errorf("PVC %s still has an active modification", claim.Name)
					}
					if className != "" {
						matched := false
						for _, status := range group.Status.Storage.VolumeAttributesClass.PVCs {
							if status.Name == claim.Name && status.UID == string(claim.UID) && status.TemplateName == template && status.DesiredClassName == className &&
								classValue(status.RequestedClassName) == className && classValue(status.CurrentClassName) == className && status.State == marklogicv1.VolumeAttributesClassCompleted {
								matched = true
							}
						}
						if !matched {
							return fmt.Errorf("PVC %s is missing from current completed CR status", claim.Name)
						}
					}
				}
			}
		}
		if fixture.cluster != "" {
			cluster := &marklogicv1.MarklogicCluster{}
			if err := run.client.Get(run.ctx, run.key(fixture.cluster), cluster); err != nil {
				return err
			}
			if err := vacProgressCurrent(cluster.Generation, cluster.Status.Storage, cluster.Status.Conditions); err != nil {
				return err
			}
			progress := cluster.Status.Storage.VolumeAttributesClass
			if len(progress.Groups) != len(expected) {
				return fmt.Errorf("parent has an incomplete child status inventory")
			}
			for _, child := range progress.Groups {
				if generation, found := generations[child.Name]; !found || child.ObservedGeneration != generation || child.State != marklogicv1.VolumeAttributesClassCompleted {
					return fmt.Errorf("parent accepted stale or incomplete child status: %s", child.Name)
				}
			}
		}
		return nil
	})
	for groupName, classes := range expected {
		run.ready(groupName, replicas)
		for template, className := range map[string]string{"datadir": classes[0], "audit": classes[1]} {
			for ordinal := int32(0); ordinal < replicas; ordinal++ {
				run.provider(run.claim(groupName, template, ordinal), className)
			}
		}
	}
}

func classValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func VACCoreFeatures(options VACOptions) []features.Feature {
	cases := []struct {
		name string
		run  func(*vacRun)
	}{
		{"initial-provisioning", (*vacRun).initialProvisioning},
		{"primary-live-change", (*vacRun).primaryLiveChange},
	}
	result := make([]features.Feature, 0, len(cases))
	for _, test := range cases {
		result = append(result, features.New("VAC core: "+test.name).
			WithLabel("type", "volume-attributes-class").WithLabel("case", test.name).
			Assess(test.name, func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
				run := newVACRun(ctx, t, config, options)
				defer run.cleanup()
				defer func() {
					if t.Failed() {
						run.diagnostics()
					}
				}()
				test.run(run)
				return ctx
			}).Feature())
	}
	return result
}

func (run *vacRun) baseline() vacFixture {
	fixture := run.cluster("cluster", run.options.StandardClass, run.options.StandardClass, nil)
	run.complete(fixture, map[string][2]string{fixture.groups[0]: {run.options.StandardClass, run.options.StandardClass}}, 1)
	return fixture
}

func (run *vacRun) initialProvisioning() {
	fixture := run.baseline()
	cluster := &marklogicv1.MarklogicCluster{}
	run.must(run.client.Get(run.ctx, run.key(fixture.cluster), cluster))
	group := run.group(fixture.groups[0])
	owner := metav1.GetControllerOf(group)
	if owner == nil || owner.UID != cluster.UID || owner.Kind != "MarklogicCluster" || owner.APIVersion != marklogicv1.GroupVersion.String() {
		run.t.Fatal("generated child has no valid cluster owner reference")
	}
	for _, claims := range []*[]corev1.PersistentVolumeClaim{cluster.Spec.AdditionalVolumeClaimTemplates, group.Spec.AdditionalVolumeClaimTemplates} {
		if claims == nil || len(*claims) != 1 || (*claims)[0].Name != "audit" || (*claims)[0].Labels["vac-e2e"] != run.prefix || (*claims)[0].Annotations["vac-e2e-metadata"] != "preserved" {
			run.t.Fatal("additional template metadata was lost during admission or inheritance")
		}
	}
	run.data(group.Name, true)
	run.data(group.Name, false)
	name := run.prefix + "-standalone"
	replicas := int32(1)
	standalone := &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.options.Namespace},
		Spec: marklogicv1.MarklogicGroupSpec{Name: name, Image: run.options.Image, Replicas: &replicas, SecretName: name + "-admin", ServiceAccountName: "default",
			Persistence: run.persistence(run.options.StandardClass), AdditionalVolumeClaimTemplates: run.additional(run.options.PerformanceClass),
			AdditionalVolumeMounts: &[]corev1.VolumeMount{{Name: "audit", MountPath: "/var/opt/MarkLogic-extra"}}}}
	run.must(run.client.Create(run.ctx, standalone))
	standaloneFixture := vacFixture{groups: []string{name}, uid: standalone.UID}
	run.fixtures = append(run.fixtures, standaloneFixture)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: standalone.Spec.SecretName, Namespace: run.options.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: marklogicv1.GroupVersion.String(), Kind: "MarklogicGroup", Name: name, UID: standalone.UID}}},
		StringData: map[string]string{"username": run.options.Username, "password": run.options.Password}}
	run.must(run.client.Create(run.ctx, secret))
	run.complete(standaloneFixture, map[string][2]string{name: {run.options.StandardClass, run.options.PerformanceClass}}, 1)
	if len(run.group(name).OwnerReferences) != 0 {
		run.t.Fatal("standalone group was unexpectedly adopted")
	}
	run.data(name, true)
	run.data(name, false)
}

func (run *vacRun) primaryLiveChange() {
	fixture := run.baseline()
	group := fixture.groups[0]
	run.data(group, true)
	before := run.snapshot(group)
	run.patchCluster(fixture, map[string]interface{}{"persistence": map[string]interface{}{"volumeAttributesClassName": run.options.PerformanceClass}})
	run.complete(fixture, map[string][2]string{group: {run.options.PerformanceClass, run.options.StandardClass}}, 1)
	run.must(vacSnapshotsStable(before, run.snapshot(group), "audit"))
	run.data(group, false)
}

func (run *vacRun) provider(claim *corev1.PersistentVolumeClaim, className string) {
	volume := &corev1.PersistentVolume{}
	run.must(run.client.Get(run.ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume))
	if volume.Spec.CSI == nil || volume.Spec.CSI.Driver != "ebs.csi.aws.com" || className == "" {
		return
	}
	profile := run.profile(className)
	parameters, _, err := unstructured.NestedStringMap(profile.Object, "parameters")
	run.must(err)
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	run.eventually("EBS applied parameters for "+claim.Name, func() error {
		ctx, cancel := context.WithTimeout(run.ctx, time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "aws", "ec2", "describe-volumes", "--region", region, "--volume-ids", volume.Spec.CSI.VolumeHandle, "--output", "json")
		command.Env = append(os.Environ(), "AWS_PAGER=")
		output, err := command.Output()
		if err != nil {
			return fmt.Errorf("describe EBS volume: %w", err)
		}
		var response struct {
			Volumes []struct {
				VolumeType string
				Iops       int64
				Throughput int64
			}
		}
		if err := json.Unmarshal(output, &response); err != nil {
			return err
		}
		if len(response.Volumes) != 1 {
			return fmt.Errorf("expected one EBS volume, got %d", len(response.Volumes))
		}
		actual := map[string]string{"type": response.Volumes[0].VolumeType, "iops": strconv.FormatInt(response.Volumes[0].Iops, 10), "throughput": strconv.FormatInt(response.Volumes[0].Throughput, 10)}
		for key, value := range actual {
			if wanted := parameters[key]; wanted != "" && wanted != value {
				return fmt.Errorf("EBS %s=%s, expected %s", key, value, wanted)
			}
		}
		return nil
	})
}

func (run *vacRun) snapshot(group string) vacSnapshot {
	snapshot := vacSnapshot{set: &appsv1.StatefulSet{}, pod: &corev1.Pod{}, claims: map[string]*corev1.PersistentVolumeClaim{}, volumes: map[string]*corev1.PersistentVolume{}}
	run.must(run.client.Get(run.ctx, run.key(group), snapshot.set))
	run.must(run.client.Get(run.ctx, run.key(group+"-0"), snapshot.pod))
	for _, template := range []string{"datadir", "audit"} {
		claim := run.claim(group, template, 0)
		snapshot.claims[template] = claim
		volume := &corev1.PersistentVolume{}
		run.must(run.client.Get(run.ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume))
		snapshot.volumes[template] = volume
	}
	return snapshot
}

func vacSnapshotsStable(before, after vacSnapshot, unchangedTemplates ...string) error {
	if before.set.UID != after.set.UID || !reflect.DeepEqual(before.set.Spec.Template, after.set.Spec.Template) ||
		!reflect.DeepEqual(before.set.Spec.VolumeClaimTemplates, after.set.Spec.VolumeClaimTemplates) || before.pod.UID != after.pod.UID ||
		before.pod.Labels["controller-revision-hash"] != after.pod.Labels["controller-revision-hash"] {
		return fmt.Errorf("VAC-only update changed StatefulSet identity/templates or Pod identity/revision")
	}
	beforeRestarts, afterRestarts := map[string]int32{}, map[string]int32{}
	for _, status := range before.pod.Status.ContainerStatuses {
		beforeRestarts[status.Name] = status.RestartCount
	}
	for _, status := range after.pod.Status.ContainerStatuses {
		afterRestarts[status.Name] = status.RestartCount
	}
	for _, status := range before.pod.Status.InitContainerStatuses {
		beforeRestarts["init/"+status.Name] = status.RestartCount
	}
	for _, status := range after.pod.Status.InitContainerStatuses {
		afterRestarts["init/"+status.Name] = status.RestartCount
	}
	if !reflect.DeepEqual(beforeRestarts, afterRestarts) {
		return fmt.Errorf("container restart counts changed")
	}
	for template, original := range before.claims {
		current := after.claims[template]
		if current == nil || original.UID != current.UID || original.Spec.VolumeName != current.Spec.VolumeName ||
			before.volumes[template].UID != after.volumes[template].UID || !reflect.DeepEqual(original.Status.Capacity, current.Status.Capacity) {
			return fmt.Errorf("PVC/PV identity, binding or capacity changed for %s", template)
		}
		originalSpec, currentSpec := original.Spec.DeepCopy(), current.Spec.DeepCopy()
		originalSpec.VolumeAttributesClassName, currentSpec.VolumeAttributesClassName = nil, nil
		if !reflect.DeepEqual(originalSpec, currentSpec) || !reflect.DeepEqual(original.OwnerReferences, current.OwnerReferences) {
			return fmt.Errorf("unrelated PVC spec or ownership changed for %s", template)
		}
		originalAnnotations, currentAnnotations := original.DeepCopy().Annotations, current.DeepCopy().Annotations
		for _, key := range []string{"marklogic.progress.com/storage-operation-id", "marklogic.progress.com/storage-operation-submitted"} {
			delete(originalAnnotations, key)
			delete(currentAnnotations, key)
		}
		if !maps.Equal(originalAnnotations, currentAnnotations) || !maps.Equal(original.Labels, current.Labels) {
			return fmt.Errorf("unrelated PVC metadata changed for %s", template)
		}
	}
	for _, template := range unchangedTemplates {
		if !reflect.DeepEqual(before.claims[template].Spec, after.claims[template].Spec) {
			return fmt.Errorf("untargeted PVC %s changed", template)
		}
	}
	return nil
}

func (run *vacRun) execute(group string, command []string) (string, error) {
	kubeClient, err := kubernetes.NewForConfig(run.config)
	if err != nil {
		return "", err
	}
	request := kubeClient.CoreV1().RESTClient().Post().Resource("pods").Namespace(run.options.Namespace).Name(group + "-0").SubResource("exec")
	request.VersionedParams(&corev1.PodExecOptions{Container: "marklogic-server", Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(run.config, "POST", request.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(run.ctx, time.Minute)
	defer cancel()
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", fmt.Errorf("exec in %s: %w: %s", group, err, stderr.String())
	}
	return stdout.String(), nil
}

func (run *vacRun) data(group string, write bool) {
	uri := "/" + run.prefix + ".json"
	// The JavaScript toObject() accessor returns an empty body on MarkLogic 12.0.3; XQuery reads the stored marker reliably.
	script := fmt.Sprintf(`fn:string(fn:doc(%q)/marker)`, uri)
	shell := `curl --fail --silent --show-error --max-time 30 --digest --user "$1:$2" --data-urlencode "xquery=$3" 'http://127.0.0.1:8000/v1/eval?database=Documents'; cat /var/opt/MarkLogic/.vac-e2e /var/opt/MarkLogic-extra/.vac-e2e`
	if write {
		script = fmt.Sprintf(`declareUpdate(); xdmp.documentInsert(%q, {marker:%q})`, uri, run.prefix)
		shell = `curl --fail --silent --show-error --max-time 30 --digest --user "$1:$2" --data-urlencode "javascript=$3" 'http://127.0.0.1:8000/v1/eval?database=Documents'; printf '%s\n' "$4" > /var/opt/MarkLogic/.vac-e2e; printf '%s\n' "$4" > /var/opt/MarkLogic-extra/.vac-e2e`
	}
	run.eventually("MarkLogic data and filesystem markers", func() error {
		output, err := run.execute(group, []string{"sh", "-ec", shell, "vac-e2e", run.options.Username, run.options.Password, script, run.prefix})
		if err != nil {
			return err
		}
		if !write && strings.Count(output, run.prefix) < 3 {
			return fmt.Errorf("application document or persisted markers missing")
		}
		return nil
	})
}

func (run *vacRun) diagnostics() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, list := range []client.ObjectList{&marklogicv1.MarklogicGroupList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PodList{}, &corev1.EventList{}} {
		if err := run.client.List(ctx, list, client.InNamespace(run.options.Namespace)); err != nil {
			run.t.Logf("VAC diagnostics %T: %v", list, err)
			continue
		}
		if groups, ok := list.(*marklogicv1.MarklogicGroupList); ok {
			for index := range groups.Items {
				groups.Items[index].Spec.Auth = nil
				groups.Items[index].Annotations = nil
			}
		}
		encoded, err := json.Marshal(list)
		if err == nil {
			run.t.Logf("VAC diagnostics %T: %s", list, encoded)
		}
	}
}

func (run *vacRun) cleanup() {
	if strings.EqualFold(os.Getenv("E2E_VAC_KEEP_RESOURCES"), "true") {
		run.t.Logf("Keeping VAC fixture %s in namespace %s; storage may continue billing", run.prefix, run.options.Namespace)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	claims := &corev1.PersistentVolumeClaimList{}
	if err := run.client.List(ctx, claims, client.InNamespace(run.options.Namespace)); err != nil {
		run.t.Errorf("list test claims for cleanup: %v", err)
	} else {
		for _, claim := range claims.Items {
			if claim.Annotations["vac-e2e-run"] == run.prefix {
				run.claims[claim.Name] = claim.UID
			}
		}
	}
	for _, fixture := range run.fixtures {
		var objects []client.Object
		if fixture.cluster != "" {
			objects = append(objects, &marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: fixture.cluster, Namespace: run.options.Namespace}})
		} else {
			for _, group := range fixture.groups {
				objects = append(objects, &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: run.options.Namespace}})
			}
		}
		for _, object := range objects {
			if err := client.IgnoreNotFound(run.client.Delete(ctx, object, client.Preconditions{UID: &fixture.uid})); err != nil {
				run.t.Errorf("delete test fixture %s: %v", object.GetName(), err)
			}
		}
		for _, group := range fixture.groups {
			err := wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(context.Context) (bool, error) {
				pods := &corev1.PodList{}
				if err := run.client.List(ctx, pods, client.InNamespace(run.options.Namespace)); err != nil {
					return false, err
				}
				for _, pod := range pods.Items {
					for _, owner := range pod.OwnerReferences {
						if owner.Kind == "StatefulSet" && owner.Name == group {
							return false, nil
						}
					}
				}
				return apierrors.IsNotFound(run.client.Get(ctx, run.key(group), &appsv1.StatefulSet{})), nil
			})
			if err != nil {
				run.t.Errorf("test workload %s did not stop; retaining its PVCs: %v", group, err)
				return
			}
		}
	}
	for name, uid := range run.claims {
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: run.options.Namespace}}
		if err := client.IgnoreNotFound(run.client.Delete(ctx, claim, client.Preconditions{UID: &uid})); err != nil {
			run.t.Errorf("delete owned test PVC %s: %v", name, err)
		}
	}
}
