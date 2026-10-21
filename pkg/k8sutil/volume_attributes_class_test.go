package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type vacDiscoveryStub struct {
	resources map[string]*metav1.APIResourceList
	errors    map[string]error
	calls     []string
}

func (stub *vacDiscoveryStub) ServerResourcesForGroupVersion(version string) (*metav1.APIResourceList, error) {
	stub.calls = append(stub.calls, version)
	if err := stub.errors[version]; err != nil {
		return nil, err
	}
	if resources, found := stub.resources[version]; found {
		return resources, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, version)
}

func vacDiscoveryFor(version string) *vacDiscoveryStub {
	return &vacDiscoveryStub{resources: map[string]*metav1.APIResourceList{
		"storage.k8s.io/" + version: {APIResources: []metav1.APIResource{{Name: "volumeattributesclasses", Kind: "VolumeAttributesClass"}}},
	}}
}

func TestVolumeAttributesClassDiscovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		discovery  *vacDiscoveryStub
		want       string
		wantReason string
		wantCalls  int
	}{
		{name: "prefer GA", discovery: vacDiscoveryFor("v1"), want: "v1", wantCalls: 1},
		{name: "beta when GA is absent", discovery: vacDiscoveryFor("v1beta1"), want: "v1beta1", wantCalls: 2},
		{name: "unsupported", discovery: &vacDiscoveryStub{}, wantReason: "VACUnsupported", wantCalls: 2},
		{name: "denied discovery is not absence", discovery: &vacDiscoveryStub{errors: map[string]error{"storage.k8s.io/v1": apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "", errors.New("denied"))}}, wantReason: "VACDiscoveryFailed", wantCalls: 1},
		{name: "timeout is not absence", discovery: &vacDiscoveryStub{errors: map[string]error{"storage.k8s.io/v1": context.DeadlineExceeded}}, wantReason: "VACDiscoveryFailed", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := NewVolumeAttributesClassValidator(nil, test.discovery)
			version, err := validator.discoverVersion()
			if version != test.want {
				t.Fatalf("version = %q, want %q", version, test.want)
			}
			assertVACValidationReason(t, err, test.wantReason)
			if len(test.discovery.calls) != test.wantCalls {
				t.Fatalf("discovery calls = %v", test.discovery.calls)
			}
			if test.wantReason == "" || test.wantReason == "VACUnsupported" {
				_, _ = validator.discoverVersion()
				if len(test.discovery.calls) != test.wantCalls {
					t.Fatal("successful or absent discovery should be cached")
				}
				validator.invalidateDiscovery()
				_, _ = validator.discoverVersion()
				if len(test.discovery.calls) != 2*test.wantCalls {
					t.Fatal("invalidated discovery was not refreshed")
				}
			}
		})
	}
}

func assertVACValidationReason(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	var failure *volumeAttributesClassValidationError
	if !errors.As(err, &failure) || failure.reason != want {
		t.Fatalf("validation error = %v, want reason %s", err, want)
	}
}

func TestVolumeAttributesClassValidation(t *testing.T) {
	for _, test := range []struct {
		name        string
		version     string
		driver      string
		provisioner string
		parameters  map[string]interface{}
		className   string
		storage     *string
		wantReason  string
	}{
		{name: "GA compatible", version: "v1", driver: "ebs.csi.aws.com", className: "profile", storage: pointerToVACStorageClass("gp3")},
		{name: "beta compatible", version: "v1beta1", driver: "ebs.csi.aws.com", className: "profile", storage: pointerToVACStorageClass("gp3")},
		{name: "custom GA driver needs no qualification allowlist", version: "v1", driver: "custom.csi.example.com", provisioner: "custom.csi.example.com", parameters: map[string]interface{}{"vendor-profile": "driver-defined", "vendor-limit": "provider-defined"}, className: "profile", storage: pointerToVACStorageClass("gp3")},
		{name: "custom beta driver needs no qualification allowlist", version: "v1beta1", driver: "custom.csi.example.com", provisioner: "custom.csi.example.com", parameters: map[string]interface{}{"vendor-profile": "driver-defined"}, className: "profile", storage: pointerToVACStorageClass("gp3")},
		{name: "default StorageClass", version: "v1", driver: "ebs.csi.aws.com", className: "profile"},
		{name: "driver mismatch", version: "v1", driver: "other.csi.example.com", className: "profile", storage: pointerToVACStorageClass("gp3"), wantReason: "VACDriverMismatch"},
		{name: "missing profile", version: "v1", driver: "ebs.csi.aws.com", className: "missing", storage: pointerToVACStorageClass("gp3"), wantReason: "VACNotFound"},
		{name: "empty driver", version: "v1", className: "profile", storage: pointerToVACStorageClass("gp3"), wantReason: "VACInvalid"},
		{name: "missing StorageClass", version: "v1", driver: "ebs.csi.aws.com", className: "profile", storage: pointerToVACStorageClass("missing"), wantReason: "VACStorageClassUnavailable"},
		{name: "explicit empty StorageClass", version: "v1", driver: "ebs.csi.aws.com", className: "profile", storage: pointerToVACStorageClass(""), wantReason: "VACStorageClassUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := storagev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			class := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "storage.k8s.io/" + test.version, "kind": "VolumeAttributesClass", "metadata": map[string]interface{}{"name": "profile"}, "driverName": test.driver}}
			if test.parameters != nil {
				class.Object["parameters"] = test.parameters
			}
			originalClass := class.DeepCopy()
			provisioner := test.provisioner
			if provisioner == "" {
				provisioner = "ebs.csi.aws.com"
			}
			storageClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: provisioner}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, storageClass).Build()
			validator := NewVolumeAttributesClassValidator(reader, vacDiscoveryFor(test.version))
			templates := []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "datadir"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: &test.className, StorageClassName: test.storage}}}
			assertVACValidationReason(t, validator.Validate(context.Background(), templates), test.wantReason)
			if err := reader.Get(context.Background(), client.ObjectKey{Name: "profile"}, class); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(class.Object["parameters"], originalClass.Object["parameters"]) {
				t.Fatal("validation must leave provider-specific parameters unchanged")
			}
		})
	}
	t.Run("unconfigured templates do not require discovery", func(t *testing.T) {
		var validator *VolumeAttributesClassValidator
		assertVACValidationReason(t, validator.Validate(context.Background(), []corev1.PersistentVolumeClaim{{}}), "")
	})
}

func TestVolumeAttributesClassDiscoveryRefresh(t *testing.T) {
	discovery := vacDiscoveryFor("v1beta1")
	validator := NewVolumeAttributesClassValidator(nil, discovery)
	if version, err := validator.discoverVersion(); err != nil || version != "v1beta1" {
		t.Fatalf("initial discovery = %s, %v", version, err)
	}
	discovery.resources = vacDiscoveryFor("v1").resources
	validator.expires = time.Now().Add(-time.Second)
	if version, err := validator.discoverVersion(); err != nil || version != "v1" {
		t.Fatalf("expired discovery did not detect GA: %s, %v", version, err)
	}
}

func TestVolumeAttributesClassReadFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		reason     string
		invalidate bool
	}{
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "profile", errors.New("denied")), reason: "VACAccessDenied"},
		{name: "unauthorized", err: apierrors.NewUnauthorized("credentials expired"), reason: "VACAccessDenied"},
		{name: "missing", err: apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "profile"), reason: "VACNotFound"},
		{name: "timeout", err: context.DeadlineExceeded, reason: "VACValidationFailed"},
		{name: "mapping changed", err: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "volumeattributesclasses"}}, reason: "VACDiscoveryFailed", invalidate: true},
		{name: "endpoint removed", err: apierrors.NewGenericServerResponse(404, "get", schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "", "endpoint missing", 0, false), reason: "VACDiscoveryFailed", invalidate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return test.err
				},
			}).Build()
			discovery := vacDiscoveryFor("v1")
			validator := NewVolumeAttributesClassValidator(reader, discovery)
			template := generatePVCTemplate(storageWithClass("profile"))
			err := validator.Validate(context.Background(), []corev1.PersistentVolumeClaim{template})
			assertVACValidationReason(t, err, test.reason)
			if !errors.Is(err, test.err) || validator.expires.IsZero() != test.invalidate || len(discovery.calls) != 1 {
				t.Fatal("read failure must preserve its cause and invalidate discovery only for mapping or endpoint changes")
			}
			for attempt := 0; attempt < 3; attempt++ {
				assertVACValidationReason(t, validator.Validate(context.Background(), []corev1.PersistentVolumeClaim{template}), test.reason)
			}
			wantCalls := 1
			if test.invalidate {
				wantCalls = 4
			}
			if len(discovery.calls) != wantCalls {
				t.Fatalf("object lookup failures must not churn discovery: calls=%d, want=%d", len(discovery.calls), wantCalls)
			}
		})
	}
}

func TestVolumeAttributesClassProvisioningValidation(t *testing.T) {
	for _, test := range []struct {
		name             string
		customDriver     bool
		driverMismatch   bool
		discoveryDenied  bool
		existing         bool
		scale            bool
		missing          bool
		unsupported      bool
		unconfigured     bool
		additionalOnly   bool
		withoutValidator bool
		prepared         bool
		templateMetadata bool
		wantReason       string
	}{
		{name: "valid initial provisioning", wantReason: "VACValidated"},
		{name: "unqualified custom driver permits provisioning", customDriver: true, wantReason: "VACValidated"},
		{name: "missing class permits initial provisioning without VAC", missing: true, wantReason: "VACNotFound"},
		{name: "unsupported API permits initial provisioning without VAC", unsupported: true, wantReason: "VACUnsupported"},
		{name: "missing validator fails closed", withoutValidator: true, wantReason: "VACDiscoveryFailed"},
		{name: "driver mismatch remains gated", driverMismatch: true, wantReason: "VACDriverMismatch"},
		{name: "driver mismatch scale-up remains gated", existing: true, scale: true, driverMismatch: true, wantReason: "VACDriverMismatch"},
		{name: "discovery denial is not unsupported", discoveryDenied: true, wantReason: "VACDiscoveryFailed"},
		{name: "unconfigured provisioning needs no VAC API", unsupported: true, unconfigured: true},
		{name: "additional class validated with primary disabled", additionalOnly: true, missing: true, wantReason: "VACNotFound"},
		{name: "additional-only unsupported provisioning continues", additionalOnly: true, unsupported: true, wantReason: "VACUnsupported"},
		{name: "additional-only unsupported scale-up continues", additionalOnly: true, existing: true, scale: true, unsupported: true, wantReason: "VACUnsupported"},
		{name: "additional-only missing scale-up continues", additionalOnly: true, existing: true, scale: true, missing: true, wantReason: "VACNotFound"},
		{name: "missing class permits scale-up without VAC", existing: true, scale: true, missing: true, wantReason: "VACNotFound"},
		{name: "prepared claims permit consumer scheduling", existing: true, scale: true, missing: true, prepared: true, wantReason: "VACNotFound"},
		{name: "unsupported API permits scale-up without VAC", existing: true, scale: true, unsupported: true, wantReason: "VACUnsupported"},
		{name: "unsupported API permits image updates without VAC", existing: true, unsupported: true, wantReason: "VACUnsupported"},
		{name: "unsupported API preserves stored template metadata", existing: true, scale: true, unsupported: true, templateMetadata: true, wantReason: "VACUnsupported"},
		{name: "unrelated image update continues", existing: true, missing: true, wantReason: "VACNotFound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
				if err := register(scheme); err != nil {
					t.Fatal(err)
				}
			}
			replicas := int32(1)
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid", Generation: 1},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &replicas, Image: "marklogic:12.0.3", Persistence: storageWithClass("profile"),
					GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}, HugePages: &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				},
			}
			if test.unconfigured {
				group.Spec.Persistence.VolumeAttributesClassName = nil
			}
			if test.additionalOnly {
				group.Spec.Persistence.Enabled = false
				additional := []corev1.PersistentVolumeClaim{generatePVCTemplate(storageWithClass("profile"))}
				additional[0].Name = "logsdir"
				group.Spec.AdditionalVolumeClaimTemplates = &additional
			}
			labels := getSelectorLabelsByComponent(group.Spec.Name, false)
			labels["app.kubernetes.io/instance"] = group.Spec.Name
			labels["app.kubernetes.io/component"] = getMarkLogicComponentLabel(false)
			current := generateStatefulSetsDef(generateObjectMeta(group.Spec.Name, group.Namespace, labels, nil), generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group)).DeepCopy()
			current.UID = "statefulset-uid"
			if test.templateMetadata {
				current.Spec.VolumeClaimTemplates[0].TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
				current.Spec.VolumeClaimTemplates[0].Status.Phase = corev1.ClaimPending
				current.Spec.VolumeClaimTemplates[0].Annotations = map[string]string{"admission.example.com/profile": "preserve"}
				mode := corev1.PersistentVolumeFilesystem
				current.Spec.VolumeClaimTemplates[0].Spec.VolumeMode = &mode
			}
			if test.scale {
				desired := int32(2)
				group.Spec.Replicas = &desired
			}
			group.Spec.Image = "marklogic:12.0.4"
			objects := []client.Object{group}
			if test.existing {
				objects = append(objects, current)
			}
			if test.prepared {
				claim := current.Spec.VolumeClaimTemplates[0].DeepCopy()
				claim.Name, claim.Namespace = "datadir-dnode-1", group.Namespace
				claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(group.UID)}
				objects = append(objects, claim)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group).WithObjects(objects...).Build()
			validator := vacValidatorForGroup(t, group)
			if test.customDriver || test.driverMismatch {
				storageScheme := runtime.NewScheme()
				if err := storagev1.AddToScheme(storageScheme); err != nil {
					t.Fatal(err)
				}
				provisioner := "custom.csi.example.com"
				if test.driverMismatch {
					provisioner = "different.csi.example.com"
				}
				validator.reader = fake.NewClientBuilder().WithScheme(storageScheme).WithObjects(
					&storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "profile"}, DriverName: "custom.csi.example.com", Parameters: map[string]string{"vendor-profile": "driver-defined"}},
					&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3"}, Provisioner: provisioner},
				).Build()
			}
			if test.missing {
				validator.reader = fake.NewClientBuilder().Build()
			}
			if test.unsupported {
				validator.discovery = &vacDiscoveryStub{}
			}
			if test.discoveryDenied {
				validator.discovery = &vacDiscoveryStub{errors: map[string]error{"storage.k8s.io/v1": apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "", errors.New("discovery access denied"))}}
			}
			if test.withoutValidator {
				validator = nil
			}
			recorder := record.NewFakeRecorder(20)
			var kubeClient client.Client = fakeClient
			if test.unsupported {
				checkRequest := func(object client.Object, patch client.Patch) error {
					switch object.(type) {
					case *appsv1.StatefulSet, *corev1.PersistentVolumeClaim:
					default:
						return nil
					}
					var encoded []byte
					var err error
					if patch != nil {
						encoded, err = patch.Data(object)
					} else {
						encoded, err = json.Marshal(object)
					}
					if err != nil {
						return err
					}
					var request map[string]any
					if err := json.Unmarshal(encoded, &request); err != nil {
						return err
					}
					var check func(any) error
					check = func(value any) error {
						switch fields := value.(type) {
						case map[string]any:
							for key, nested := range fields {
								if key == "volumeAttributesClassName" {
									return fmt.Errorf("unsupported cluster received VAC field in %T request", object)
								}
								if err := check(nested); err != nil {
									return err
								}
							}
						case []any:
							for _, nested := range fields {
								if err := check(nested); err != nil {
									return err
								}
							}
						}
						return nil
					}
					return check(request)
				}
				kubeClient = interceptor.NewClient(fakeClient, interceptor.Funcs{
					Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if err := checkRequest(object, nil); err != nil {
							return err
						}
						return underlying.Create(ctx, object, options...)
					},
					Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if err := checkRequest(object, nil); err != nil {
							return err
						}
						return underlying.Update(ctx, object, options...)
					},
					Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						if err := checkRequest(object, patch); err != nil {
							return err
						}
						return underlying.Patch(ctx, object, patch, options...)
					},
				})
			}
			operator := &OperatorContext{Ctx: context.Background(), Client: kubeClient, Scheme: scheme, MarklogicGroup: group, Recorder: recorder, VACValidator: validator}
			if _, err := operator.ReconcileStatefulset(); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(group.Status.Conditions, VolumeAttributesClassValidationCondition)
			if test.unconfigured {
				if condition != nil {
					t.Fatal("unconfigured resources should not get a VAC condition")
				}
			} else if condition == nil || condition.Reason != test.wantReason || condition.ObservedGeneration != group.Generation {
				t.Fatalf("unexpected validation condition: %+v", condition)
			}
			if test.wantReason == "VACValidated" && (condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, "CSI capabilities and parameter acceptance are not prevalidated")) {
				t.Fatalf("validation success must identify its limits: %+v", condition)
			}
			actual := &appsv1.StatefulSet{}
			err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual)
			fallback := test.wantReason == "VACUnsupported" || test.wantReason == "VACNotFound"
			blocked := test.wantReason != "" && test.wantReason != "VACValidated" && !fallback
			if blocked && !test.existing {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("invalid explicit selection must not create a StatefulSet: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				wantReplicas := int32(1)
				if test.prepared || test.scale && fallback {
					wantReplicas = 2
				}
				if *actual.Spec.Replicas != wantReplicas || actual.Spec.Template.Spec.Containers[0].Image != group.Spec.Image {
					t.Fatal("VAC fallback must allow scale-up; other validation errors must still allow unrelated workload updates")
				}
				if fallback && !test.existing {
					for _, template := range actual.Spec.VolumeClaimTemplates {
						if template.Spec.VolumeAttributesClassName != nil {
							t.Fatal("fallback initial templates must omit VAC")
						}
					}
				}
				if fallback && test.existing && !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
					t.Fatal("fallback must preserve immutable historical templates")
				}
			}
			claims := &corev1.PersistentVolumeClaimList{}
			wantClaims := 0
			if test.prepared || test.scale && fallback {
				wantClaims = 1
			}
			if err := fakeClient.List(operator.Ctx, claims); err != nil || len(claims.Items) != wantClaims {
				t.Fatalf("unexpected PVC creation: %+v, %v", claims.Items, err)
			}
			if fallback {
				if !strings.Contains(condition.Message, "To fix:") || !strings.Contains(condition.Message, "without this VAC") {
					t.Fatal("fallback condition must explain default provisioning and remediation")
				}
				if !test.additionalOnly && *group.Spec.Persistence.VolumeAttributesClassName != "profile" {
					t.Fatal("fallback must not mutate the desired CR selection")
				}
				for _, claim := range claims.Items {
					if !test.prepared && claim.Spec.VolumeAttributesClassName != nil {
						t.Fatal("new fallback claims must omit VAC despite historical templates")
					}
					if test.prepared && (claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != "profile") {
						t.Fatal("prepared existing PVC references must remain unchanged")
					}
				}
				warning := false
				for len(recorder.Events) > 0 {
					event := <-recorder.Events
					warning = warning || strings.Contains(event, "Warning "+test.wantReason) && strings.Contains(event, "To fix:")
				}
				if !warning {
					t.Fatal("fallback must emit an actionable Warning event")
				}
			}
			for len(recorder.Events) > 0 {
				<-recorder.Events
			}
			if test.existing && (blocked || fallback) {
				if _, err := operator.ReconcileStatefulset(); err != nil {
					t.Fatal(err)
				}
				if len(recorder.Events) != 0 {
					t.Fatal("unchanged validation must not repeat Events")
				}
			}
			if test.missing {
				operator.VACValidator = vacValidatorForGroup(t, group)
				if _, err := operator.ReconcileStatefulset(); err != nil {
					t.Fatal(err)
				}
				condition = meta.FindStatusCondition(group.Status.Conditions, VolumeAttributesClassValidationCondition)
				if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "VACValidated" || len(recorder.Events) == 0 {
					t.Fatal("remediation must clear validation failure and emit a transition Event")
				}
				group.Spec.Persistence.VolumeAttributesClassName = nil
				group.Spec.AdditionalVolumeClaimTemplates = nil
				if _, err := operator.validateVolumeAttributesClassTemplates(nil); err != nil {
					t.Fatal(err)
				}
				if meta.FindStatusCondition(group.Status.Conditions, VolumeAttributesClassValidationCondition) != nil {
					t.Fatal("opt-out must remove the validation condition")
				}
			}
		})
	}
}

func TestVolumeAttributesClassProvisioningFallbackTemplates(t *testing.T) {
	for _, scale := range []bool{false, true} {
		for _, missing := range []string{"datadir", "logsdir"} {
			t.Run(fmt.Sprintf("scale=%t/missing=%s", scale, missing), func(t *testing.T) {
				scheme := runtime.NewScheme()
				for _, register := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
					if err := register(scheme); err != nil {
						t.Fatal(err)
					}
				}
				replicas := int32(1)
				logs := generatePVCTemplate(storageWithClass("logs-profile"))
				logs.Name = "logsdir"
				group := &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid", Generation: 1}, Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &replicas, Image: "marklogic:12.0.3", Persistence: storageWithClass("profile"), AdditionalVolumeClaimTemplates: &[]corev1.PersistentVolumeClaim{logs},
					GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}, HugePages: &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				}}
				labels := getSelectorLabelsByComponent(group.Spec.Name, false)
				labels["app.kubernetes.io/instance"], labels["app.kubernetes.io/component"] = group.Spec.Name, getMarkLogicComponentLabel(false)
				current := generateStatefulSetsDef(generateObjectMeta(group.Spec.Name, group.Namespace, labels, nil), generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group)).DeepCopy()
				objects := []client.Object{group}
				if scale {
					objects = append(objects, current)
					desired := int32(2)
					group.Spec.Replicas = &desired
				}
				fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group).WithObjects(objects...).Build()
				validator := vacValidatorForGroup(t, group)
				missingClass := "profile"
				if missing == "logsdir" {
					missingClass = "logs-profile"
				}
				if err := validator.reader.(client.Client).Delete(context.Background(), &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: missingClass}}); err != nil {
					t.Fatal(err)
				}
				operator := &OperatorContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: record.NewFakeRecorder(20), VACValidator: validator}
				if _, err := operator.ReconcileStatefulset(); err != nil {
					t.Fatal(err)
				}
				actual := &appsv1.StatefulSet{}
				if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
					t.Fatal(err)
				}
				if *actual.Spec.Replicas != *group.Spec.Replicas {
					t.Fatal("missing one template class must not prevent provisioning or scale-up")
				}
				for _, template := range actual.Spec.VolumeClaimTemplates {
					if !scale && template.Name == missing {
						if template.Spec.VolumeAttributesClassName != nil {
							t.Fatal("only the missing class must be omitted")
						}
					} else if template.Spec.VolumeAttributesClassName == nil || (template.Name == "datadir" && *template.Spec.VolumeAttributesClassName != "profile") || (template.Name == "logsdir" && *template.Spec.VolumeAttributesClassName != "logs-profile") {
						t.Fatal("healthy selections and historical immutable templates must remain intact")
					}
				}
				if *group.Spec.Persistence.VolumeAttributesClassName != "profile" || *(*group.Spec.AdditionalVolumeClaimTemplates)[0].Spec.VolumeAttributesClassName != "logs-profile" {
					t.Fatal("fallback must preserve independent desired CR selections")
				}
				claims := &corev1.PersistentVolumeClaimList{}
				if err := fakeClient.List(operator.Ctx, claims); err != nil {
					t.Fatal(err)
				}
				if scale {
					if len(claims.Items) != 1 || claims.Items[0].Name != missing+"-dnode-1" || claims.Items[0].Spec.VolumeAttributesClassName != nil {
						t.Fatal("scale-up must precreate only the fallback template without VAC")
					}
				} else if len(claims.Items) != 0 {
					t.Fatal("initial provisioning must remain StatefulSet-driven")
				}
			})
		}
	}
}

func liveVolumeAttributesClassFixture(t *testing.T) (*OperatorContext, *appsv1.StatefulSet, corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme, storagev1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	replicas := int32(1)
	group := &marklogicv1.MarklogicGroup{
		TypeMeta:   metav1.TypeMeta{APIVersion: marklogicv1.GroupVersion.String(), Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid", Generation: 1},
		Spec:       marklogicv1.MarklogicGroupSpec{Name: "dnode", Replicas: &replicas, Persistence: storageWithClass("premium")},
	}
	template := generatePVCTemplate(group.Spec.Persistence)
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: group.Namespace, UID: "sts-uid", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(group, marklogicv1.GroupVersion.WithKind("MarklogicGroup"))}},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, VolumeClaimTemplates: []corev1.PersistentVolumeClaim{template}},
	}
	claim := template.DeepCopy()
	claim.Name, claim.Namespace, claim.UID = "datadir-dnode-0", group.Namespace, "claim-uid"
	claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(group.UID)}
	claim.Spec.VolumeName = "pv-data"
	claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("standard")
	claim.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, CurrentVolumeAttributesClassName: pointerToVACStorageClass("standard"), Capacity: claim.Spec.Resources.Requests.DeepCopy()}
	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: claim.Spec.VolumeName, UID: "pv-uid"},
		Spec:       corev1.PersistentVolumeSpec{StorageClassName: "gp3", ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "test-volume"}}},
	}
	profile := &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "premium"}, DriverName: "ebs.csi.aws.com", Parameters: map[string]string{"profile": "premium"}}
	storageClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3"}, Provisioner: "ebs.csi.aws.com"}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group, claim).
		WithIndex(&corev1.PersistentVolumeClaim{}, PrecreatedPVCGroupUIDIndex, PrecreatedPVCGroupUIDs).
		WithIndex(&corev1.Pod{}, PodPVCReferenceIndex, PodPVCReferences).
		WithObjects(group, statefulSet, claim, volume, profile, storageClass).Build()
	operator := &OperatorContext{Ctx: context.Background(), Client: fakeClient, APIReader: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: record.NewFakeRecorder(20), VACValidator: NewVolumeAttributesClassValidator(fakeClient, vacDiscoveryFor("v1"))}
	return operator, statefulSet, template, claim
}

func TestUnusedPrecreatedPVCReporting(t *testing.T) {
	operator, statefulSet, template, _ := liveVolumeAttributesClassFixture(t)
	desired := int32(2)
	operator.MarklogicGroup.Spec.Replicas = &desired
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	claim := template.DeepCopy()
	claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "unused-uid"
	claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
	if err := operator.Client.Create(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	before := claim.DeepCopy()
	observe := func() *marklogicv1.MarklogicGroup {
		t.Helper()
		if next, err := operator.observeUnusedPrecreatedPVCs(); err != nil || next.RequeueAfter != 30*time.Second {
			t.Fatalf("unused claim observation = %+v, %v", next, err)
		}
		saved := &marklogicv1.MarklogicGroup{}
		if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	first := observe()
	if first.Status.Storage == nil || len(first.Status.Storage.UnusedPVCs) != 1 || first.Status.Storage.UnusedPVCs[0].Reason != "ScaleUpPending" || meta.FindStatusCondition(first.Status.Conditions, StorageClaimsInUseCondition).Status != metav1.ConditionFalse {
		t.Fatalf("missing unused claim report: %+v", first.Status)
	}
	recorder := operator.Recorder.(*record.FakeRecorder)
	if len(recorder.Events) != 1 || !strings.Contains(<-recorder.Events, "Warning UnusedPrecreatedPVC") {
		t.Fatal("unused claim must emit a warning")
	}
	second := observe()
	if second.ResourceVersion != first.ResourceVersion || len(recorder.Events) != 0 {
		t.Fatal("unchanged unused claim must not rewrite status or repeat warnings")
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil || !reflect.DeepEqual(claim, before) {
		t.Fatalf("reporting must not mutate the PVC: %v", err)
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(statefulSet), statefulSet); err != nil {
		t.Fatal(err)
	}
	statefulSet.Spec.Replicas = &desired
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	resolved := observe()
	if resolved.Status.Storage != nil || meta.FindStatusCondition(resolved.Status.Conditions, StorageClaimsInUseCondition) != nil {
		t.Fatal("successful scale-up must clear the unused claim report")
	}
	if len(recorder.Events) != 1 || !strings.Contains(<-recorder.Events, "Normal UnusedPrecreatedPVCsResolved") {
		t.Fatal("reuse must emit one resolution event")
	}
}

func TestUnusedPrecreatedPVCSafety(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OperatorContext, *appsv1.StatefulSet, *corev1.PersistentVolumeClaim)
		want   bool
	}{
		{name: "cancelled scale-up retains candidate", want: true},
		{name: "bound candidate may contain data", want: true, mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Spec.VolumeName, claim.Status.Phase = "pv-unused", corev1.ClaimBound
		}},
		{name: "nonzero ordinal start", want: true, mutate: func(_ *OperatorContext, set *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			set.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: 5}
			claim.Name = "datadir-dnode-6"
		}},
		{name: "additional template", want: true, mutate: func(_ *OperatorContext, set *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			logs := set.Spec.VolumeClaimTemplates[0].DeepCopy()
			logs.Name = "logsdir"
			set.Spec.VolumeClaimTemplates = append(set.Spec.VolumeClaimTemplates, *logs)
			claim.Name = "logsdir-dnode-1"
		}},
		{name: "VAC opt-out retains unused reporting", want: true, mutate: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) {
			operator.MarklogicGroup.Spec.Persistence.VolumeAttributesClassName = nil
		}},
		{name: "ordinary retained claim is not operator precreated", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Annotations = nil
		}},
		{name: "foreign group marker", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Annotations[dataPVCGroupUIDAnnotation] = "another-group"
		}},
		{name: "foreign owner", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "dnode", UID: "another-set"}}
		}},
		{name: "other namespace", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Namespace = "other-ns"
		}},
		{name: "noncanonical ordinal", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Name = "datadir-dnode-01"
		}},
		{name: "undeclared template", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Name = "external-dnode-1"
		}},
		{name: "active replica is not unused", mutate: func(operator *OperatorContext, set *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) {
			replicas := int32(2)
			set.Spec.Replicas, operator.MarklogicGroup.Spec.Replicas = &replicas, &replicas
		}},
		{name: "terminating claim", mutate: func(_ *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			now := metav1.Now()
			claim.DeletionTimestamp, claim.Finalizers = &now, []string{"kubernetes.io/pvc-protection"}
		}},
		{name: "referenced by an unrelated pending Pod", mutate: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: claim.Namespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}}}
			if err := operator.Client.Create(operator.Ctx, pod); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, set, template, active := liveVolumeAttributesClassFixture(t)
			active.Annotations = nil
			if err := operator.Client.Update(operator.Ctx, active); err != nil {
				t.Fatal(err)
			}
			claim := template.DeepCopy()
			claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "unused-uid"
			claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
			if test.mutate != nil {
				test.mutate(operator, set, claim)
			}
			if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
				t.Fatal(err)
			}
			if err := operator.Client.Update(operator.Ctx, set); err != nil {
				t.Fatal(err)
			}
			terminating := claim.DeletionTimestamp != nil
			if err := operator.Client.Create(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			if terminating {
				if err := operator.Client.Delete(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
			}
			before := claim.DeepCopy()
			if _, err := operator.observeUnusedPrecreatedPVCs(); err != nil {
				t.Fatal(err)
			}
			if operator.MarklogicGroup.Spec.Persistence.VolumeAttributesClassName == nil {
				if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
					t.Fatal(err)
				}
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			var records []marklogicv1.UnusedPVCStatus
			if saved.Status.Storage != nil {
				records = saved.Status.Storage.UnusedPVCs
			}
			if (len(records) != 0) != test.want {
				t.Fatalf("unexpected unused candidates: %+v", records)
			}
			if test.want && (len(records) != 1 || records[0].Name != claim.Name || records[0].UID != string(claim.UID) || records[0].Reason != "OutsideReplicaRange" || records[0].FirstObservedTime.IsZero()) {
				t.Fatalf("missing candidate identity or reason: %+v", records)
			}
			if test.want && claim.Status.Phase == corev1.ClaimBound && !strings.Contains(records[0].Message, "may contain data") {
				t.Fatal("bound candidate must warn about data")
			}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil || !reflect.DeepEqual(claim, before) {
				t.Fatalf("reporting changed or deleted a claim: %v", err)
			}
		})
	}
}

func TestUnusedPrecreatedPVCReadBudget(t *testing.T) {
	operator, _, template, _ := liveVolumeAttributesClassFixture(t)
	base := operator.Client
	for ordinal := 0; ordinal < 200; ordinal++ {
		for _, object := range []client.Object{
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("unrelated-%d", ordinal), Namespace: operator.MarklogicGroup.Namespace}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("unrelated-%d", ordinal), Namespace: operator.MarklogicGroup.Namespace}},
		} {
			if err := base.Create(operator.Ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	lists, liveClaims := 0, 0
	operator.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			listing := &client.ListOptions{}
			for _, option := range options {
				option.ApplyToList(listing)
			}
			if listing.FieldSelector == nil || listing.FieldSelector.Empty() {
				t.Fatal("unused reporting must not scan a whole namespace")
			}
			lists++
			return underlying.List(ctx, list, options...)
		},
	})
	operator.APIReader = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("unused reporting must not send namespace-wide List requests to the API server")
			return nil
		},
		Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, claim := object.(*corev1.PersistentVolumeClaim); claim {
				liveClaims++
			}
			return underlying.Get(ctx, key, object, options...)
		},
	})
	if result, err := operator.observeUnusedPrecreatedPVCs(); err != nil || result.RequeueAfter != 0 || lists != 1 || liveClaims != 0 {
		t.Fatalf("active marked claims must not poll or trigger live claim reads: result=%+v lists=%d gets=%d error=%v", result, lists, liveClaims, err)
	}
	claim := template.DeepCopy()
	claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "unused-uid"
	claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
	if err := base.Create(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	lists, liveClaims = 0, 0
	if result, err := operator.observeUnusedPrecreatedPVCs(); err != nil || result.RequeueAfter != 30*time.Second || lists != 2 || liveClaims != 1 {
		t.Fatalf("candidate reporting must have bounded indexed reads: result=%+v lists=%d gets=%d error=%v", result, lists, liveClaims, err)
	}
}

func TestUnusedPrecreatedPVCObservationFailure(t *testing.T) {
	for _, failPods := range []bool{false, true} {
		t.Run(fmt.Sprintf("pods=%t", failPods), func(t *testing.T) {
			operator, _, template, _ := liveVolumeAttributesClassFixture(t)
			claim := template.DeepCopy()
			claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "unused-uid"
			claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
			if err := operator.Client.Create(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			if _, err := operator.observeUnusedPrecreatedPVCs(); err != nil {
				t.Fatal(err)
			}
			recorder := operator.Recorder.(*record.FakeRecorder)
			<-recorder.Events
			cached := operator.Client
			operator.Client = interceptor.NewClient(cached.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
					_, pods := list.(*corev1.PodList)
					if pods == failPods {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "observation"}, "", errors.New("denied"))
					}
					return underlying.List(ctx, list, options...)
				},
			})
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := operator.observeUnusedPrecreatedPVCs(); !apierrors.IsForbidden(err) {
					t.Fatalf("expected observation failure, got %v", err)
				}
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(saved.Status.Conditions, StorageClaimsInUseCondition)
			if condition == nil || condition.Status != metav1.ConditionUnknown || len(saved.Status.Storage.UnusedPVCs) != 1 || len(recorder.Events) != 1 {
				t.Fatalf("lookup uncertainty must preserve candidates and emit one warning: %+v", saved.Status)
			}
			if event := <-recorder.Events; !strings.Contains(event, "Warning PrecreatedPVCObservationFailed") {
				t.Fatal(event)
			}
			operator.Client = cached
			if _, err := operator.observeUnusedPrecreatedPVCs(); err != nil {
				t.Fatal(err)
			}
			if len(recorder.Events) != 0 {
				t.Fatal("restoring observation must not repeat an unchanged candidate warning")
			}
		})
	}
}

func TestUnusedPrecreatedPVCStatusSafety(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(fmt.Sprintf("superseded=%t", superseded), func(t *testing.T) {
			operator, _, template, _ := liveVolumeAttributesClassFixture(t)
			claim := template.DeepCopy()
			claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "unused-uid"
			claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
			if err := operator.Client.Create(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
					if _, pods := list.(*corev1.PodList); pods && superseded {
						latest := &marklogicv1.MarklogicGroup{}
						if err := underlying.Get(ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), latest); err != nil {
							return err
						}
						latest.Generation++
						if err := underlying.Update(ctx, latest); err != nil {
							return err
						}
					}
					return underlying.List(ctx, list, options...)
				},
				SubResourcePatch: func(ctx context.Context, underlying client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
					attempts++
					if attempts == 1 {
						latest := &marklogicv1.MarklogicGroup{}
						if err := underlying.Get(ctx, client.ObjectKeyFromObject(object), latest); err != nil {
							return err
						}
						latest.Status.Stage = "ConcurrentUpdate"
						latest.Status.Storage = &marklogicv1.StorageStatus{VolumeAttributesClass: &marklogicv1.VolumeAttributesClassStatus{State: marklogicv1.VolumeAttributesClassPending, Reason: "KeepVAC", Message: "Preserve VAC progress"}}
						latest.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{OperationID: "keep-resize"}
						if err := underlying.Status().Update(ctx, latest); err != nil {
							return err
						}
						return apierrors.NewConflict(schema.GroupResource{Group: marklogicv1.GroupVersion.Group, Resource: "marklogicgroups"}, object.GetName(), errors.New("status changed"))
					}
					return underlying.SubResource(subresource).Patch(ctx, object, patch, options...)
				},
			})
			operator.APIReader = operator.Client
			if _, err := operator.observeUnusedPrecreatedPVCs(); err != nil {
				t.Fatal(err)
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			if superseded {
				if attempts != 0 || saved.Status.Storage != nil || len(operator.Recorder.(*record.FakeRecorder).Events) != 0 {
					t.Fatal("superseded observations must not publish status or Events")
				}
			} else if attempts != 2 || saved.Status.Stage != "ConcurrentUpdate" || saved.Status.VolumeResizeStatus.OperationID != "keep-resize" || saved.Status.Storage.VolumeAttributesClass.Reason != "KeepVAC" || len(saved.Status.Storage.UnusedPVCs) != 1 || len(operator.Recorder.(*record.FakeRecorder).Events) != 1 {
				t.Fatalf("conflict retry must preserve unrelated status and emit one warning: %+v", saved.Status)
			}
		})
	}
}

func TestVolumeAttributesClassBoundPVCUpdate(t *testing.T) {
	for _, test := range []struct {
		name        string
		pending     bool
		unsupported bool
	}{
		{name: "bound field-only patch"},
		{name: "unbound claim stays unchanged", pending: true},
		{name: "unsupported API never patches a bound PVC", unsupported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
			patches := 0
			if test.unsupported {
				operator.VACValidator.discovery = &vacDiscoveryStub{}
				operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						patches++
						return underlying.Patch(ctx, object, patch, options...)
					},
				})
			}
			if test.pending {
				claim.Status.Phase = corev1.ClaimPending
				if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			before := &corev1.PersistentVolumeClaim{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), before); err != nil {
				t.Fatal(err)
			}
			submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, before)
			if test.unsupported {
				var failure *volumeAttributesClassValidationError
				if submitted || patches != 0 || !errors.As(err, &failure) || failure.reason != "VACUnsupported" {
					t.Fatalf("unsupported API must skip native VAC writes: submitted=%v, patches=%d, err=%v", submitted, patches, err)
				}
			} else if err != nil || submitted == test.pending {
				t.Fatalf("submitted=%v, error=%v", submitted, err)
			}
			after := &corev1.PersistentVolumeClaim{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), after); err != nil {
				t.Fatal(err)
			}
			want := before.DeepCopy()
			if !test.pending && !test.unsupported {
				want.Spec.VolumeAttributesClassName = template.Spec.VolumeAttributesClassName
				group := &marklogicv1.MarklogicGroup{}
				if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
					t.Fatal(err)
				}
				operation := storageOperation(group)
				if operation == nil || operation.ID == "" || operation.PVCUID != string(before.UID) {
					t.Fatal("VAC submission requires a persisted identity-bound operation guard")
				}
				want.Annotations[storageOperationAnnotation] = operation.ID
			}
			if !reflect.DeepEqual(after.Spec, want.Spec) || !reflect.DeepEqual(after.Status, before.Status) || after.UID != before.UID || !reflect.DeepEqual(after.Annotations, want.Annotations) || !reflect.DeepEqual(after.OwnerReferences, before.OwnerReferences) {
				t.Fatal("VAC submission must preserve every unrelated PVC field and identity")
			}
			if test.unsupported {
				return
			}
			if submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, after); err != nil || submitted {
				t.Fatalf("repeated reconciliation must not resubmit: %v", err)
			}
		})
	}
}

func TestVolumeAttributesClassConvergesAfterBinding(t *testing.T) {
	operator, _, _, claim := liveVolumeAttributesClassFixture(t)
	claim.Status.Phase = corev1.ClaimPending
	if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	if next, err := operator.ReconcileVolumeAttributesClasses(); err != nil || next.RequeueAfter == 0 {
		t.Fatalf("unbound claim should be observed without patching: %+v, %v", next, err)
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	if *claim.Spec.VolumeAttributesClassName != "standard" {
		t.Fatal("an unbound claim's original class must be preserved")
	}
	operator.MarklogicGroup.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("newest")
	operator.MarklogicGroup.Generation++
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	if err := operator.Client.Create(operator.Ctx, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "newest"}, DriverName: "ebs.csi.aws.com", Parameters: map[string]string{"profile": "newest"}}); err != nil {
		t.Fatal(err)
	}
	claim.Status.Phase = corev1.ClaimBound
	if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := operator.ReconcileVolumeAttributesClasses(); err != nil {
		t.Fatal(err)
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	if *claim.Spec.VolumeAttributesClassName != "newest" || *claim.Status.CurrentVolumeAttributesClassName != "standard" {
		t.Fatal("bound claim must request the latest target without claiming CSI application")
	}
}

func TestVolumeAttributesClassAdditionalLiveConvergence(t *testing.T) {
	operator, statefulSet, template, primary := liveVolumeAttributesClassFixture(t)
	additional := *template.DeepCopy()
	additional.Name = "logsdir"
	additional.Spec.VolumeAttributesClassName = pointerToVACStorageClass("logs-premium")
	operator.MarklogicGroup.Spec.AdditionalVolumeClaimTemplates = &[]corev1.PersistentVolumeClaim{additional}
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, additional)
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	logs := primary.DeepCopy()
	logs.Name, logs.UID, logs.Spec.VolumeName = "logsdir-dnode-0", "logs-uid", "pv-logs"
	volume := &corev1.PersistentVolume{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: primary.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	volume.Name, volume.UID, volume.ResourceVersion = logs.Spec.VolumeName, "pv-logs-uid", ""
	volume.Spec.ClaimRef.Name, volume.Spec.ClaimRef.UID = logs.Name, logs.UID
	logs.ResourceVersion = ""
	for _, object := range []client.Object{logs, volume, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "logs-premium"}, DriverName: "ebs.csi.aws.com", Parameters: map[string]string{"profile": "logs"}}} {
		if err := operator.Client.Create(operator.Ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := operator.ReconcileVolumeAttributesClasses(); err != nil {
		t.Fatal(err)
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(primary), primary); err != nil {
		t.Fatal(err)
	}
	if *primary.Spec.VolumeAttributesClassName != "premium" {
		t.Fatal("primary must use its own target")
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(logs), logs); err != nil {
		t.Fatal(err)
	}
	if *logs.Spec.VolumeAttributesClassName != "standard" {
		t.Fatal("only one live update may be submitted before CSI application")
	}
	primary.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Status().Update(operator.Ctx, primary); err != nil {
		t.Fatal(err)
	}
	if _, err := operator.ReconcileVolumeAttributesClasses(); err != nil {
		t.Fatal(err)
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(logs), logs); err != nil {
		t.Fatal(err)
	}
	if *logs.Spec.VolumeAttributesClassName != "logs-premium" {
		t.Fatal("additional claims must converge to their independent target")
	}
}

func TestVolumeAttributesClassLiveSafety(t *testing.T) {
	for _, test := range []struct {
		name           string
		prepare        func(*OperatorContext, *appsv1.StatefulSet, *corev1.PersistentVolumeClaim) error
		wantError      bool
		wantSubmission bool
	}{
		{name: "first selection on legacy bound claim", wantSubmission: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Spec.VolumeAttributesClassName = nil
			return operator.Client.Update(operator.Ctx, claim)
		}},
		{name: "ownerless legacy claim requires managed Pod", wantError: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Annotations = nil
			return operator.Client.Update(operator.Ctx, claim)
		}},
		{name: "ownerless claim verified through managed Pod", wantSubmission: true, prepare: func(operator *OperatorContext, statefulSet *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Annotations = nil
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				return err
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dnode-0", Namespace: claim.Namespace, UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "datadir", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}}}
			return operator.Client.Create(operator.Ctx, pod)
		}},
		{name: "foreign group UID", wantError: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Annotations[dataPVCGroupUIDAnnotation] = "foreign-group"
			return operator.Client.Update(operator.Ctx, claim)
		}},
		{name: "foreign owner despite matching annotation", wantError: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "other", UID: "foreign-uid"}}
			return operator.Client.Update(operator.Ctx, claim)
		}},
		{name: "claimed group owner", wantSubmission: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Annotations = nil
			claim.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(operator.MarklogicGroup, marklogicv1.GroupVersion.WithKind("MarklogicGroup"))}
			return operator.Client.Update(operator.Ctx, claim)
		}},
		{name: "bound PV driver mismatch", wantError: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			volume := &corev1.PersistentVolume{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
				return err
			}
			volume.Spec.CSI.Driver = "other.csi.example.com"
			return operator.Client.Update(operator.Ctx, volume)
		}},
		{name: "bound PV claim UID mismatch", wantError: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			volume := &corev1.PersistentVolume{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
				return err
			}
			volume.Spec.ClaimRef.UID = "replaced-claim-uid"
			return operator.Client.Update(operator.Ctx, volume)
		}},
		{name: "resize operation active", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) error {
			operator.MarklogicGroup.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{Phase: marklogicv1.VolumeResizePhaseWaitingForPVCResize}
			return operator.Client.Status().Update(operator.Ctx, operator.MarklogicGroup)
		}},
		{name: "native resize active", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionTrue}}
			return operator.Client.Status().Update(operator.Ctx, claim)
		}},
		{name: "native modification pending", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "standard", Status: "Pending"}
			return operator.Client.Status().Update(operator.Ctx, claim)
		}},
		{name: "native modification unknown", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "standard", Status: "FutureStatus"}
			return operator.Client.Status().Update(operator.Ctx, claim)
		}},
		{name: "infeasible previous target can be corrected", wantSubmission: true, prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) error {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "standard", Status: "Infeasible"}
			return operator.Client.Status().Update(operator.Ctx, claim)
		}},
		{name: "superseded group generation", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) error {
			latest := operator.MarklogicGroup.DeepCopy()
			latest.Generation++
			return operator.Client.Update(operator.Ctx, latest)
		}},
		{name: "new target read before patch", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) error {
			latest := operator.MarklogicGroup.DeepCopy()
			latest.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("newest")
			return operator.Client.Update(operator.Ctx, latest)
		}},
		{name: "opt-out never clears existing class", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) error {
			latest := operator.MarklogicGroup.DeepCopy()
			latest.Spec.Persistence.VolumeAttributesClassName = nil
			return operator.Client.Update(operator.Ctx, latest)
		}},
		{name: "inactive after scale-down", prepare: func(operator *OperatorContext, _ *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) error {
			latest := operator.MarklogicGroup.DeepCopy()
			zero := int32(0)
			latest.Spec.Replicas = &zero
			return operator.Client.Update(operator.Ctx, latest)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if err := test.prepare(operator, statefulSet, claim); err != nil {
				t.Fatal(err)
			}
			submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, claim)
			if submitted != test.wantSubmission || (err != nil) != test.wantError {
				t.Fatalf("submitted=%v, error=%v", submitted, err)
			}
			actual := &corev1.PersistentVolumeClaim{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), actual); err != nil {
				t.Fatal(err)
			}
			if test.wantSubmission && (actual.Spec.VolumeAttributesClassName == nil || *actual.Spec.VolumeAttributesClassName != "premium") {
				t.Fatal("verified bound claim did not request its desired class")
			}
			if !test.wantSubmission && !reflect.DeepEqual(actual.Spec.VolumeAttributesClassName, claim.Spec.VolumeAttributesClassName) {
				t.Fatal("ineligible claim was modified")
			}
		})
	}
}

func TestVolumeAttributesClassConflictRetry(t *testing.T) {
	operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
	base := operator.Client
	patches := 0
	operator.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			patches++
			data, err := patch.Data(object)
			if err != nil {
				return err
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(data, &payload); err != nil {
				return err
			}
			spec, ok := payload["spec"].(map[string]interface{})
			metadata, metaOK := payload["metadata"].(map[string]interface{})
			if !ok || len(spec) != 1 || spec["volumeAttributesClassName"] != "premium" || !metaOK || metadata["resourceVersion"] == nil {
				return fmt.Errorf("patch must contain only the VAC field and optimistic resourceVersion: %s", data)
			}
			if patches == 1 {
				latest := &corev1.PersistentVolumeClaim{}
				if err := underlying.Get(ctx, client.ObjectKeyFromObject(object), latest); err != nil {
					return err
				}
				latest.Annotations["concurrent-change"] = "preserved"
				if err := underlying.Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName(), errors.New("concurrent update"))
			}
			return underlying.Patch(ctx, object, patch, options...)
		},
	})
	if submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, claim); err != nil || !submitted || patches != 2 {
		t.Fatalf("conflict was not retried with a fresh read: submitted=%v, patches=%d, err=%v", submitted, patches, err)
	}
	actual := &corev1.PersistentVolumeClaim{}
	if err := base.Get(operator.Ctx, client.ObjectKeyFromObject(claim), actual); err != nil {
		t.Fatal(err)
	}
	if actual.Annotations["concurrent-change"] != "preserved" {
		t.Fatal("conflict retry overwrote a concurrent change")
	}
}

func TestVolumeAttributesClassNativeOperationCorrelation(t *testing.T) {
	for _, test := range []struct {
		name         string
		requested    string
		current      string
		modification *corev1.ModifyVolumeStatus
		pending      bool
	}{
		{name: "already applied", requested: "standard", current: "standard"},
		{name: "submitted but not applied", requested: "premium", current: "standard", pending: true},
		{name: "infeasible current target is terminal", requested: "premium", current: "standard", modification: &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "Infeasible"}},
		{name: "old infeasible status cannot clear a new request", requested: "premium", current: "standard", modification: &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "standard", Status: "Infeasible"}, pending: true},
		{name: "native in-progress remains active", requested: "premium", current: "premium", modification: &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "InProgress"}, pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: &test.requested}, Status: corev1.PersistentVolumeClaimStatus{CurrentVolumeAttributesClassName: &test.current, ModifyVolumeStatus: test.modification}}
			if pending := volumeAttributesClassOperationPending(claim); pending != test.pending {
				t.Fatalf("pending=%v, want %v", pending, test.pending)
			}
		})
	}
}

func TestVolumeAttributesClassDroppedField(t *testing.T) {
	operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
	operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return nil
		},
	})
	if attempted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, claim); err == nil || !attempted || !strings.Contains(err.Error(), "did not retain requested") {
		t.Fatalf("missing persisted field must be reported after a write attempt: attempted=%v, err=%v", attempted, err)
	}
}

func TestVolumeAttributesClassAdmissionTargetMustPersist(t *testing.T) {
	operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
	writes := 0
	operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
				writes++
			}
			return underlying.Patch(ctx, object, patch, options...)
		},
		SubResourcePatch: func(ctx context.Context, underlying client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if group, ok := object.(*marklogicv1.MarklogicGroup); ok && storageOperation(group) != nil {
				storageOperation(group).AdmissionTarget = nil
			}
			return underlying.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	for pass := 0; pass < 2; pass++ {
		attempted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, claim)
		var failure *volumeAttributesClassValidationError
		if attempted || !errors.As(err, &failure) || failure.reason != "VACAdmissionStateNotPersisted" {
			t.Fatalf("pruned admission target must prevent dispatch: attempted=%t, err=%v", attempted, err)
		}
	}
	if writes != 0 {
		t.Fatalf("PVC writes without durable admission intent = %d, want 0", writes)
	}
}

func TestVolumeAttributesClassAdmissionFailureSuppression(t *testing.T) {
	for _, nilReference := range []bool{false, true} {
		t.Run(fmt.Sprintf("nilReference=%t", nilReference), func(t *testing.T) {
			operator, _, _, claim := liveVolumeAttributesClassFixture(t)
			if nilReference {
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				claim.Spec.VolumeAttributesClassName = nil
				if err := operator.Client.Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
				claim.Status.CurrentVolumeAttributesClassName = nil
				if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok {
						writes++
						saved := &corev1.PersistentVolumeClaim{}
						if err := underlying.Get(ctx, client.ObjectKeyFromObject(pvc), saved); err != nil {
							return err
						}
						pvc.Spec.VolumeAttributesClassName = saved.Spec.VolumeAttributesClassName
					}
					return underlying.Patch(ctx, object, patch, options...)
				},
			})
			for pass := 0; pass < 4; pass++ {
				fresh := &marklogicv1.MarklogicGroup{}
				if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), fresh); err != nil {
					t.Fatal(err)
				}
				restarted := *operator
				restarted.MarklogicGroup = fresh
				restarted.VACValidator = NewVolumeAttributesClassValidator(operator.VACValidator.reader, vacDiscoveryFor("v1"))
				operator = &restarted
				_, dispatchErr := operator.ReconcileVolumeAttributesClasses()
				var failure *volumeAttributesClassValidationError
				if !errors.As(dispatchErr, &failure) || failure.reason != "VACFieldNotPersisted" {
					t.Fatalf("admission failure must remain diagnostic, got %v", dispatchErr)
				}
				if _, err := operator.observeVolumeAttributesClasses(dispatchErr); err != nil {
					t.Fatal(err)
				}
			}
			if writes != 1 {
				t.Fatalf("the same rejected target must cause only one PVC write across restarts, got %d", writes)
			}
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			if saved.Status.Storage == nil || saved.Status.Storage.VolumeAttributesClass == nil || saved.Status.Storage.VolumeAttributesClass.Reason != "VACFieldNotPersisted" {
				t.Fatal("read-only observation must retain the persisted admission failure")
			}
		})
	}
}

func TestVolumeAttributesClassAdmissionFailureRetryBoundaries(t *testing.T) {
	updateGroup := func(t *testing.T, operator *OperatorContext, mutate func(*marklogicv1.MarklogicGroup)) {
		t.Helper()
		group := &marklogicv1.MarklogicGroup{}
		if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
			t.Fatal(err)
		}
		mutate(group)
		if err := operator.Client.Update(operator.Ctx, group); err != nil {
			t.Fatal(err)
		}
		operator.MarklogicGroup = group
	}
	for _, test := range []struct {
		name            string
		mutate          func(*testing.T, *OperatorContext, *corev1.PersistentVolumeClaim)
		wantRetry       bool
		failStatusWrite bool
		pruneLatch      bool
		rejectRetry     bool
		completed       bool
	}{
		{name: "unrelated group generation does not retry", mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) { group.Generation++; group.Spec.Image = "new-image" })
		}},
		{name: "class metadata revision does not retry", mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			profile := &storagev1.VolumeAttributesClass{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: "premium"}, profile); err != nil {
				t.Fatal(err)
			}
			profile.Annotations = map[string]string{"unrelated": "updated"}
			if err := operator.Client.Update(operator.Ctx, profile); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "explicit capability retry", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) {
				group.Annotations = map[string]string{volumeAttributesClassAdmissionRetryAnnotation: "after-admission-repair"}
			})
		}},
		{name: "desired target change", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			profile := &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "next", UID: "next-uid"}, DriverName: "ebs.csi.aws.com"}
			if err := operator.Client.Create(operator.Ctx, profile); err != nil {
				t.Fatal(err)
			}
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) {
				group.Generation++
				group.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("next")
			})
		}},
		{name: "class recreation", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			profile := &storagev1.VolumeAttributesClass{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: "premium"}, profile); err != nil {
				t.Fatal(err)
			}
			if err := operator.Client.Delete(operator.Ctx, profile); err != nil {
				t.Fatal(err)
			}
			profile.UID, profile.ResourceVersion = "replacement-class-uid", ""
			if err := operator.Client.Create(operator.Ctx, profile); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "served API version change", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			reader := interceptor.NewClient(operator.VACValidator.reader.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if class, ok := object.(*unstructured.Unstructured); ok && class.GetKind() == "VolumeAttributesClass" {
						version := class.GroupVersionKind()
						class.SetGroupVersionKind(storagev1.SchemeGroupVersion.WithKind("VolumeAttributesClass"))
						err := underlying.Get(ctx, key, object, options...)
						class.SetGroupVersionKind(version)
						return err
					}
					return underlying.Get(ctx, key, object, options...)
				},
			})
			operator.VACValidator = NewVolumeAttributesClassValidator(reader, vacDiscoveryFor("v1beta1"))
		}},
		{name: "PVC replacement", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, claim *corev1.PersistentVolumeClaim) {
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if err := operator.Client.Delete(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.UID, claim.ResourceVersion = "replacement-pvc-uid", ""
			if err := operator.Client.Create(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			volume := &corev1.PersistentVolume{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
				t.Fatal(err)
			}
			volume.Spec.ClaimRef.UID = claim.UID
			if err := operator.Client.Update(operator.Ctx, volume); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "scale down does not erase rejection", mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) { replicas := int32(0); group.Spec.Replicas = &replicas })
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) { replicas := int32(1); group.Spec.Replicas = &replicas })
		}},
		{name: "opt out and reselect permits retry", wantRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) { group.Spec.Persistence.VolumeAttributesClassName = nil })
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) {
				group.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("premium")
			})
		}},
		{name: "external native convergence remains observable", completed: true, mutate: func(t *testing.T, operator *OperatorContext, claim *corev1.PersistentVolumeClaim) {
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("premium")
			if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "temporary latch persistence failure does not resubmit", failStatusWrite: true},
		{name: "pruned latch keeps the guard held", pruneLatch: true},
		{name: "rejected explicit retry is latched again", wantRetry: true, rejectRetry: true, mutate: func(t *testing.T, operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			updateGroup(t, operator, func(group *marklogicv1.MarklogicGroup) {
				group.Annotations = map[string]string{volumeAttributesClassAdmissionRetryAnnotation: "retry-still-rejected"}
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, _, _, claim := liveVolumeAttributesClassFixture(t)
			writes, statusFailed := 0, false
			admit := false
			operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok {
						writes++
						if !admit {
							saved := &corev1.PersistentVolumeClaim{}
							if err := underlying.Get(ctx, client.ObjectKeyFromObject(pvc), saved); err != nil {
								return err
							}
							pvc.Spec.VolumeAttributesClassName = saved.Spec.VolumeAttributesClassName
						}
					}
					return underlying.Patch(ctx, object, patch, options...)
				},
				SubResourcePatch: func(ctx context.Context, underlying client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
					if group, ok := object.(*marklogicv1.MarklogicGroup); ok && group.Status.Storage != nil && len(group.Status.Storage.VolumeAttributesClassAdmissionFailures) > 0 && test.failStatusWrite && !statusFailed {
						statusFailed = true
						return apierrors.NewServiceUnavailable("temporary admission-latch status failure")
					}
					if group, ok := object.(*marklogicv1.MarklogicGroup); ok && group.Status.Storage != nil && test.pruneLatch {
						group.Status.Storage.VolumeAttributesClassAdmissionFailures = nil
					}
					return underlying.SubResource(subresource).Patch(ctx, object, patch, options...)
				},
			})
			if _, err := operator.ReconcileVolumeAttributesClasses(); err == nil {
				t.Fatal("initial rejected admission must be reported")
			}
			if test.mutate != nil {
				test.mutate(t, operator, claim)
			}
			admit = !test.rejectRetry
			for pass := 0; pass < 2; pass++ {
				fresh := &marklogicv1.MarklogicGroup{}
				if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), fresh); err != nil {
					t.Fatal(err)
				}
				restarted := *operator
				restarted.MarklogicGroup = fresh
				operator = &restarted
				_, dispatchErr := operator.ReconcileVolumeAttributesClasses()
				if (test.wantRetry && !test.rejectRetry || test.completed) && dispatchErr != nil {
					t.Fatal(dispatchErr)
				}
				if _, err := operator.observeVolumeAttributesClasses(dispatchErr); err != nil {
					t.Fatal(err)
				}
			}
			wantWrites := 1
			if test.wantRetry {
				wantWrites++
			}
			if writes != wantWrites {
				t.Fatalf("PVC writes = %d, want %d", writes, wantWrites)
			}
			if test.completed {
				saved := &marklogicv1.MarklogicGroup{}
				if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
					t.Fatal(err)
				}
				if saved.Status.Storage.VolumeAttributesClass.State != marklogicv1.VolumeAttributesClassCompleted {
					t.Fatal("native convergence must replace the failure diagnostic")
				}
			}
		})
	}
}

func TestStorageOperationStaleCache(t *testing.T) {
	operator, set, template, claim := liveVolumeAttributesClassFixture(t)
	stale := claim.DeepCopy()
	if attempted, err := operator.reconcileBoundVolumeAttributesClass(set, template, 0, claim); err != nil || !attempted {
		t.Fatalf("request VAC: attempted=%t error=%v", attempted, err)
	}
	fresh := &marklogicv1.MarklogicGroup{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Spec.Persistence.Size = "40Gi"
	fresh.Spec.UpdateStrategy = appsv1.OnDeleteStatefulSetStrategyType
	fresh.Generation++
	if err := operator.Client.Update(operator.Ctx, fresh); err != nil {
		t.Fatal(err)
	}
	operator.MarklogicGroup = fresh
	underlying := operator.Client
	operator.Client = interceptor.NewClient(underlying.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, next client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok && key.Name == stale.Name {
				stale.DeepCopyInto(pvc)
				return nil
			}
			return next.Get(ctx, key, object, options...)
		},
	})
	operator.APIReader = underlying
	operator.ReconcileVolumeResizeValidation()
	saved := &marklogicv1.MarklogicGroup{}
	if err := underlying.Get(operator.Ctx, client.ObjectKeyFromObject(fresh), saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status.VolumeResizeStatus != nil {
		t.Fatal("stale PVC cache must not allow resize while live VAC work is pending")
	}
}

func TestStorageOperationCAS(t *testing.T) {
	for _, resizeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resizeFirst=%t", resizeFirst), func(t *testing.T) {
			operator, _, _, claim := liveVolumeAttributesClassFixture(t)
			group := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
				t.Fatal(err)
			}
			resize := &marklogicv1.VolumeResizeStatus{OperationID: "resize-test", Phase: marklogicv1.VolumeResizePhaseValidating}
			if resizeFirst {
				if acquired, err := operator.claimResizeStatusCAS(resize); err != nil || !acquired {
					t.Fatalf("claim resize: acquired=%t error=%v", acquired, err)
				}
				if _, _, err := operator.claimVACStorageOperation(group, claim, nil); !apierrors.IsConflict(err) {
					t.Fatalf("stale VAC acquisition must conflict with resize: %v", err)
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(group), group); err != nil {
					t.Fatal(err)
				}
				if _, operation, err := operator.claimVACStorageOperation(group, claim, nil); err != nil || operation != nil {
					t.Fatalf("active resize must prevent VAC acquisition: %v", err)
				}
			} else {
				if _, operation, err := operator.claimVACStorageOperation(group, claim, nil); err != nil || operation == nil {
					t.Fatalf("claim VAC: %v", err)
				}
				if acquired, err := operator.claimResizeStatusCAS(resize); err != nil || acquired {
					t.Fatalf("VAC acquisition must exclude resize: acquired=%t error=%v", acquired, err)
				}
			}
		})
	}
}

func TestStorageOperationConcurrentClaims(t *testing.T) {
	operator, _, _, claim := liveVolumeAttributesClassFixture(t)
	group := &marklogicv1.MarklogicGroup{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for ordinal := 0; ordinal < 2; ordinal++ {
		worker := &OperatorContext{Ctx: operator.Ctx, Client: operator.Client, APIReader: operator.APIReader, MarklogicGroup: group.DeepCopy()}
		target := claim.DeepCopy()
		target.Name = fmt.Sprintf("datadir-dnode-%d", ordinal)
		target.UID = types.UID(fmt.Sprintf("claim-%d", ordinal))
		snapshot := group.DeepCopy()
		go func() {
			<-start
			_, operation, err := worker.claimVACStorageOperation(snapshot, target, nil)
			if err == nil && operation == nil {
				err = errors.New("missing operation")
			}
			results <- err
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for count := 0; count < 2; count++ {
		err := <-results
		if err == nil {
			successes++
		} else if apierrors.IsConflict(err) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent acquisitions must have one owner: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestStorageOperationInterruptedWrite(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprintf("applied=%t", applied), func(t *testing.T) {
			operator, set, template, claim := liveVolumeAttributesClassFixture(t)
			base := operator.Client
			before := &corev1.PersistentVolumeClaim{}
			if err := base.Get(operator.Ctx, client.ObjectKeyFromObject(claim), before); err != nil {
				t.Fatal(err)
			}
			operator.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if applied {
						if err := underlying.Patch(ctx, object, patch, options...); err != nil {
							return err
						}
					}
					return apierrors.NewTimeoutError("ambiguous response", 1)
				},
			})
			if attempted, err := operator.reconcileBoundVolumeAttributesClass(set, template, 0, before); err == nil || !attempted {
				t.Fatalf("expected ambiguous write: attempted=%t error=%v", attempted, err)
			}
			operator.Client = base
			group := &marklogicv1.MarklogicGroup{}
			if err := base.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
				t.Fatal(err)
			}
			operation := storageOperation(group).DeepCopy()
			group.Spec.Persistence.VolumeAttributesClassName = nil
			group.Generation++
			if err := base.Update(operator.Ctx, group); err != nil {
				t.Fatal(err)
			}
			restarted := &OperatorContext{Ctx: operator.Ctx, Client: base, APIReader: base, MarklogicGroup: group}
			active, err := restarted.reconcileStorageOperation()
			if err != nil || active != applied {
				t.Fatalf("recovery: active=%t applied=%t error=%v", active, applied, err)
			}
			if !applied {
				late := before.DeepCopy()
				late.Spec.VolumeAttributesClassName = template.Spec.VolumeAttributesClassName
				late.Annotations[storageOperationAnnotation] = operation.ID
				if err := base.Patch(operator.Ctx, late, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
					t.Fatalf("late VAC write must be fenced: %v", err)
				}
			} else {
				if err := base.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				claim.Status.CurrentVolumeAttributesClassName = template.Spec.VolumeAttributesClassName
				if err := base.Status().Update(operator.Ctx, claim); err != nil {
					t.Fatal(err)
				}
				if active, err := restarted.reconcileStorageOperation(); err != nil || active {
					t.Fatalf("settled VAC must release its guard: active=%t error=%v", active, err)
				}
			}
		})
	}
}

func TestVolumeAttributesClassResizeInterlock(t *testing.T) {
	operator, statefulSet, template, claim := liveVolumeAttributesClassFixture(t)
	if submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, template, 0, claim); err != nil || !submitted {
		t.Fatalf("VAC request not submitted: %v", err)
	}
	operator.MarklogicGroup.Spec.Persistence.Size = "40Gi"
	resize := operator.ReconcileVolumeResizeValidation()
	if !resize.Completed() {
		t.Fatal("new resize must defer while a VAC request is unapplied")
	}
	if next, err := resize.Output(); err != nil || next.RequeueAfter == 0 {
		t.Fatalf("resize must requeue for VAC observation: %+v, %v", next, err)
	}
	if operator.MarklogicGroup.Status.VolumeResizeStatus != nil {
		t.Fatal("waiting for VAC must not claim a new resize operation")
	}
	actual := &corev1.PersistentVolumeClaim{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual.Spec.Resources, claim.Spec.Resources) {
		t.Fatal("VAC observation must not submit the requested size change")
	}
}

func TestVolumeAttributesClassAmbiguousWriteStopsDispatch(t *testing.T) {
	operator, statefulSet, _, claim := liveVolumeAttributesClassFixture(t)
	replicas := int32(2)
	operator.MarklogicGroup.Spec.Replicas = &replicas
	statefulSet.Spec.Replicas = &replicas
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	second := claim.DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "datadir-dnode-1", "second-claim-uid", ""
	second.Spec.VolumeName = "pv-second"
	volume := &corev1.PersistentVolume{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	volume.Name, volume.UID, volume.ResourceVersion = second.Spec.VolumeName, "second-pv-uid", ""
	volume.Spec.ClaimRef.Name, volume.Spec.ClaimRef.UID = second.Name, second.UID
	for _, object := range []client.Object{second, volume} {
		if err := operator.Client.Create(operator.Ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	patches := 0
	operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			patches++
			if err := underlying.Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			return apierrors.NewTimeoutError("response lost after successful patch", 1)
		},
	})
	if _, err := operator.ReconcileVolumeAttributesClasses(); err == nil || patches != 1 {
		t.Fatalf("ambiguous write must stop group dispatch: patches=%d, err=%v", patches, err)
	}
	if err := operator.APIReader.Get(operator.Ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatal(err)
	}
	if *second.Spec.VolumeAttributesClassName != "standard" {
		t.Fatal("a second claim must not be patched after an ambiguous first write")
	}
}

func pointerToVACStorageClass(value string) *string { return &value }

func TestVolumeAttributesClassNativeProgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.PersistentVolumeClaim)
		state  marklogicv1.VolumeAttributesClassState
		reason string
	}{
		{name: "applied", state: marklogicv1.VolumeAttributesClassCompleted, reason: "VACApplied"},
		{name: "saved is not applied", mutate: func(claim *corev1.PersistentVolumeClaim) { claim.Status.CurrentVolumeAttributesClassName = nil }, state: marklogicv1.VolumeAttributesClassPending, reason: "VACAwaitingApplication"},
		{name: "binding mismatch", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Phase = corev1.ClaimPending
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("old")
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "PendingClassMismatch"},
		{name: "native pending", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "Pending"}
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "VACPending"},
		{name: "native progress", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "InProgress"}
		}, state: marklogicv1.VolumeAttributesClassInProgress, reason: "VACInProgress"},
		{name: "infeasible", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "Infeasible"}
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "VACInfeasible"},
		{name: "old target failure", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "old", Status: "Infeasible"}
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "PreviousTargetPending"},
		{name: "unknown native", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "FutureState"}
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACNativeStateUnknown"},
		{name: "provider error", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: "ModifyVolumeError", Status: corev1.ConditionTrue, Reason: "ProviderCooldown", Message: "retry after cooldown"}}
		}, state: marklogicv1.VolumeAttributesClassInProgress, reason: "ProviderCooldown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv", VolumeAttributesClassName: pointerToVACStorageClass("premium")}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, CurrentVolumeAttributesClassName: pointerToVACStorageClass("premium")}}
			if test.mutate != nil {
				test.mutate(claim)
			}
			state, reason, message := volumeAttributesClassNativeProgress(claim, "premium")
			if state != test.state || reason != test.reason || message == "" {
				t.Fatalf("progress = %s/%s/%s, want %s/%s", state, reason, message, test.state, test.reason)
			}
		})
	}
}

func TestVolumeAttributesClassPendingSavedReference(t *testing.T) {
	for _, test := range []struct {
		name           string
		savedDriver    string
		missingDesired bool
		lookupErr      error
		mutate         func(*testing.T, *OperatorContext, *appsv1.StatefulSet, *corev1.PersistentVolumeClaim)
		state          marklogicv1.VolumeAttributesClassState
		reason         string
		message        string
		recover        bool
	}{
		{name: "missing original class", state: marklogicv1.VolumeAttributesClassFailed, reason: "ProvisioningBlocked", message: "VolumeAttributesClass standard", recover: true},
		{name: "valid original class", savedDriver: "ebs.csi.aws.com", state: marklogicv1.VolumeAttributesClassPending, reason: "PendingClassMismatch"},
		{name: "both classes missing", missingDesired: true, state: marklogicv1.VolumeAttributesClassFailed, reason: "ProvisioningBlocked", message: "VolumeAttributesClass standard"},
		{name: "only desired class missing", savedDriver: "ebs.csi.aws.com", missingDesired: true, state: marklogicv1.VolumeAttributesClassFailed, reason: "VACNotFound", message: "VolumeAttributesClass premium"},
		{name: "original class read forbidden", lookupErr: apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, "standard", errors.New("denied")), state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACAccessDenied", message: "Saved PVC class reference"},
		{name: "original class read unauthorized", lookupErr: apierrors.NewUnauthorized("authentication failed"), state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACAccessDenied", message: "Saved PVC class reference"},
		{name: "original class read timeout", lookupErr: apierrors.NewTimeoutError("temporary lookup timeout", 1), state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACValidationFailed", message: "temporary lookup timeout"},
		{name: "original class endpoint unavailable", lookupErr: apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "volumeattributesclasses"}, ""), state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACDiscoveryFailed", message: "Saved PVC class reference"},
		{name: "original class driver mismatch", savedDriver: "other.csi.example", state: marklogicv1.VolumeAttributesClassFailed, reason: "VACDriverMismatch", message: "VolumeAttributesClass standard"},
		{name: "no saved selection", mutate: func(t *testing.T, operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Spec.VolumeAttributesClassName = nil
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "PendingClassMismatch"},
		{name: "same saved and desired missing class", missingDesired: true, mutate: func(t *testing.T, operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "ProvisioningBlocked", message: "VolumeAttributesClass premium"},
		{name: "foreign claim beats invalid desired class", missingDesired: true, mutate: func(t *testing.T, operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Annotations[dataPVCGroupUIDAnnotation] = "foreign-group"
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "PVCOwnershipUnverified"},
		{name: "terminating claim beats invalid desired class", missingDesired: true, mutate: func(t *testing.T, operator *OperatorContext, _ *appsv1.StatefulSet, claim *corev1.PersistentVolumeClaim) {
			claim.Finalizers = []string{"test.example/retain"}
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			if err := operator.Client.Delete(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "PVCTerminating"},
		{name: "foreign workload beats invalid desired class", missingDesired: true, mutate: func(t *testing.T, operator *OperatorContext, set *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) {
			set.OwnerReferences = nil
			if err := operator.Client.Update(operator.Ctx, set); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "WorkloadUnverified"},
		{name: "terminating workload beats invalid desired class", missingDesired: true, mutate: func(t *testing.T, operator *OperatorContext, set *appsv1.StatefulSet, _ *corev1.PersistentVolumeClaim) {
			set.Finalizers = []string{"test.example/retain"}
			if err := operator.Client.Update(operator.Ctx, set); err != nil {
				t.Fatal(err)
			}
			if err := operator.Client.Delete(operator.Ctx, set); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "WorkloadUnverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, set, _, claim := liveVolumeAttributesClassFixture(t)
			claim.Spec.VolumeName = ""
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}
			if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			if test.savedDriver != "" {
				if err := operator.Client.Create(operator.Ctx, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, DriverName: test.savedDriver}); err != nil {
					t.Fatal(err)
				}
			}
			if test.missingDesired {
				if err := operator.Client.Delete(operator.Ctx, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "premium"}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.lookupErr != nil {
				operator.VACValidator.reader = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, ok := object.(*unstructured.Unstructured); ok && key.Name == "standard" {
							return test.lookupErr
						}
						return underlying.Get(ctx, key, object, options...)
					},
				})
			}
			if test.mutate != nil {
				test.mutate(t, operator, set, claim)
			}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			before := claim.DeepCopy()
			saved := &marklogicv1.MarklogicGroup{}
			previousVersion := ""
			for pass := 0; pass < 2; pass++ {
				if next, err := operator.observeVolumeAttributesClasses(nil); err != nil || next.RequeueAfter != 30*time.Second {
					t.Fatalf("observe = %+v, %v", next, err)
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
					t.Fatal(err)
				}
				progress := saved.Status.Storage.VolumeAttributesClass
				if progress.State != test.state || progress.Reason != test.reason || len(progress.PVCs) != 1 || progress.PVCs[0].Reason != test.reason || !strings.Contains(progress.PVCs[0].Message, test.message) {
					t.Fatalf("unexpected saved-reference diagnostic: %+v", progress)
				}
				if pass > 0 && saved.ResourceVersion != previousVersion {
					t.Fatal("unchanged observations must not rewrite status")
				}
				previousVersion = saved.ResourceVersion
			}
			if test.recover {
				if err := operator.Client.Create(operator.Ctx, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, DriverName: "ebs.csi.aws.com"}); err != nil {
					t.Fatal(err)
				}
				if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
					t.Fatal(err)
				}
				if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
					t.Fatal(err)
				}
				if progress := saved.Status.Storage.VolumeAttributesClass; progress.State != marklogicv1.VolumeAttributesClassPending || progress.Reason != "PendingClassMismatch" {
					t.Fatalf("repairing the original prerequisite must clear ProvisioningBlocked: %+v", progress)
				}
			}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, claim) {
				t.Fatal("observation must leave the Pending PVC unchanged")
			}
		})
	}
}

func TestVolumeAttributesClassProgressReporting(t *testing.T) {
	operator, _, _, claim := liveVolumeAttributesClassFixture(t)
	group := operator.MarklogicGroup.DeepCopy()
	group.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}
	group.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{OperationID: "keep-resize"}
	group.Status.Dynamic = &marklogicv1.DynamicGroupStatus{Configured: true, Phase: "Configured"}
	if err := operator.Client.Status().Update(operator.Ctx, group); err != nil {
		t.Fatal(err)
	}
	read := func() *marklogicv1.MarklogicGroup {
		t.Helper()
		current := &marklogicv1.MarklogicGroup{}
		if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(group), current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	observe := func() *marklogicv1.MarklogicGroup {
		t.Helper()
		if result, err := operator.observeVolumeAttributesClasses(nil); err != nil || result.RequeueAfter != 30*time.Second {
			t.Fatalf("observe = %v, %v", result, err)
		}
		return read()
	}
	first := observe()
	progress := first.Status.Storage.VolumeAttributesClass
	if progress.State != marklogicv1.VolumeAttributesClassPending || len(progress.PVCs) != 1 || progress.PVCs[0].Reason != "VACNotRequested" || meta.FindStatusCondition(first.Status.Conditions, VolumeAttributesClassSyncedCondition).Status != metav1.ConditionFalse {
		t.Fatalf("unexpected initial status: %+v", progress)
	}
	second := observe()
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatal("unchanged observations must not write status")
	}
	claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	if status := observe().Status.Storage.VolumeAttributesClass; status.State == marklogicv1.VolumeAttributesClassCompleted {
		t.Fatal("a saved request must not report completion")
	}
	claim.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	completed := observe()
	if completed.Status.Storage.VolumeAttributesClass.State != marklogicv1.VolumeAttributesClassCompleted || meta.FindStatusCondition(completed.Status.Conditions, VolumeAttributesClassSyncedCondition).Status != metav1.ConditionTrue {
		t.Fatal("applied desired class must complete")
	}
	if completed.Status.VolumeResizeStatus.OperationID != "keep-resize" || completed.Status.Dynamic == nil || !completed.Status.Dynamic.Configured || meta.FindStatusCondition(completed.Status.Conditions, "Ready") == nil {
		t.Fatal("VAC reporting must preserve unrelated status")
	}
	recorder := operator.Recorder.(*record.FakeRecorder)
	if len(recorder.Events) != 1 {
		t.Fatalf("want one completion event, got %d", len(recorder.Events))
	}
	observe()
	if len(recorder.Events) != 1 {
		t.Fatal("unchanged completion must not repeat Events")
	}
	completed.Spec.Persistence.VolumeAttributesClassName = nil
	if err := operator.Client.Update(operator.Ctx, completed); err != nil {
		t.Fatal(err)
	}
	operator.MarklogicGroup = completed
	if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
		t.Fatal(err)
	}
	cleared := read()
	if cleared.Status.Storage != nil || meta.FindStatusCondition(cleared.Status.Conditions, VolumeAttributesClassSyncedCondition) != nil {
		t.Fatal("opt-out must remove only active VAC progress")
	}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	if *claim.Spec.VolumeAttributesClassName != "premium" {
		t.Fatal("reporting must never mutate PVC references")
	}
}

func TestVolumeAttributesClassModifyErrorEvents(t *testing.T) {
	operator, _, _, claim := liveVolumeAttributesClassFixture(t)
	recorder := operator.Recorder.(*record.FakeRecorder)
	previousVersion := ""
	for _, test := range []struct {
		name   string
		mutate func(*corev1.PersistentVolumeClaim)
		state  marklogicv1.VolumeAttributesClassState
		event  string
		noOp   bool
	}{
		{name: "ordinary progress", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Type = "ModifyingVolume"
		}, state: marklogicv1.VolumeAttributesClassInProgress},
		{name: "error onset with same reason", state: marklogicv1.VolumeAttributesClassInProgress, event: "VolumeAttributesClassModifyError"},
		{name: "repeated error", state: marklogicv1.VolumeAttributesClassInProgress, noOp: true},
		{name: "message change only", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Message = "cooldown still active"
		}, state: marklogicv1.VolumeAttributesClassInProgress},
		{name: "error reason change", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Reason = "ProviderBusy"
		}, state: marklogicv1.VolumeAttributesClassInProgress, event: "VolumeAttributesClassModifyError"},
		{name: "recovery with same reason", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Type = "ModifyingVolume"
			claim.Status.Conditions[0].Reason = "ProviderBusy"
		}, state: marklogicv1.VolumeAttributesClassInProgress},
		{name: "error reappears", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Reason = "ProviderBusy"
		}, state: marklogicv1.VolumeAttributesClassInProgress, event: "VolumeAttributesClassModifyError"},
		{name: "inactive error", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Conditions[0].Status = corev1.ConditionFalse
		}, state: marklogicv1.VolumeAttributesClassInProgress},
		{name: "previous requested class", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("standard")
		}, state: marklogicv1.VolumeAttributesClassPending},
		{name: "previous native target", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus.TargetVolumeAttributesClassName = "standard"
		}, state: marklogicv1.VolumeAttributesClassPending},
		{name: "current target error", state: marklogicv1.VolumeAttributesClassInProgress, event: "VolumeAttributesClassModifyError"},
		{name: "infeasible uses existing failure event", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.ModifyVolumeStatus.Status = "Infeasible"
		}, state: marklogicv1.VolumeAttributesClassFailed, event: "VolumeAttributesClassFailed"},
	} {
		if !t.Run(test.name, func(t *testing.T) {
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "InProgress"}
			claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: "ModifyVolumeError", Status: corev1.ConditionTrue, Reason: "ProviderCooldown", Message: "retry after cooldown"}}
			if test.mutate != nil {
				test.mutate(claim)
			}
			conditions, modification := claim.Status.Conditions, claim.Status.ModifyVolumeStatus
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.Conditions, claim.Status.ModifyVolumeStatus = conditions, modification
			if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			group := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
				t.Fatal(err)
			}
			progress := group.Status.Storage.VolumeAttributesClass
			if progress.State != test.state || progress.PVCs[0].State != test.state || meta.FindStatusCondition(group.Status.Conditions, VolumeAttributesClassSyncedCondition).Status != metav1.ConditionFalse {
				t.Fatalf("unexpected progress or synced condition: %+v", group.Status)
			}
			wantError := test.state == marklogicv1.VolumeAttributesClassInProgress && conditions[0].Type == "ModifyVolumeError" && conditions[0].Status == corev1.ConditionTrue
			if progress.PVCs[0].ModifyVolumeError != wantError {
				t.Fatalf("modifyVolumeError = %t, want %t", progress.PVCs[0].ModifyVolumeError, wantError)
			}
			if test.noOp && group.ResourceVersion != previousVersion {
				t.Fatal("unchanged error must not rewrite status")
			}
			previousVersion = group.ResourceVersion
			if test.event == "" {
				if len(recorder.Events) != 0 {
					t.Fatalf("unexpected event: %s", <-recorder.Events)
				}
				return
			}
			if len(recorder.Events) != 1 {
				t.Fatalf("want one %s warning, got %d events", test.event, len(recorder.Events))
			}
			event := <-recorder.Events
			if !strings.HasPrefix(event, "Warning "+test.event+" ") || !strings.Contains(event, claim.Name) || !strings.Contains(event, "premium") || !strings.Contains(event, conditions[0].Message) {
				t.Fatalf("warning lost error context: %s", event)
			}
			if test.state == marklogicv1.VolumeAttributesClassInProgress && (progress.PVCs[0].Reason != conditions[0].Reason || progress.PVCs[0].Message != conditions[0].Message || !strings.Contains(event, conditions[0].Reason)) {
				t.Fatalf("provider diagnostics not preserved: progress=%+v, event=%s", progress, event)
			}
		}) {
			break
		}
	}
}

func TestVolumeAttributesClassModifyErrorEventTransitions(t *testing.T) {
	previous := &marklogicv1.VolumeAttributesClassStatus{PVCs: []marklogicv1.PVCVolumeAttributesClassStatus{{
		Name: "data-0", UID: "original", DesiredClassName: "premium", ModifyVolumeError: true,
		State: marklogicv1.VolumeAttributesClassInProgress, Reason: "ProviderCooldown", Message: "retry after cooldown",
	}}}
	for _, test := range []struct {
		name   string
		mutate func(*marklogicv1.PVCVolumeAttributesClassStatus)
		event  string
	}{
		{name: "unchanged error"},
		{name: "replacement PVC", mutate: func(status *marklogicv1.PVCVolumeAttributesClassStatus) {
			status.UID = "replacement"
		}, event: "VolumeAttributesClassModifyError"},
		{name: "new target", mutate: func(status *marklogicv1.PVCVolumeAttributesClassStatus) {
			status.DesiredClassName = "new-premium"
		}, event: "VolumeAttributesClassModifyError"},
		{name: "stall emits only existing stall warning", mutate: func(status *marklogicv1.PVCVolumeAttributesClassStatus) {
			status.State = marklogicv1.VolumeAttributesClassStalled
		}, event: "VolumeAttributesClassStalled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := previous.DeepCopy()
			if test.mutate != nil {
				test.mutate(&current.PVCs[0])
			}
			recorder := record.NewFakeRecorder(2)
			emitVolumeAttributesClassProgressEvents(recorder, &marklogicv1.MarklogicGroup{}, previous, current)
			if test.event == "" {
				if len(recorder.Events) != 0 {
					t.Fatal("unchanged native error must not repeat its warning")
				}
				return
			}
			if len(recorder.Events) != 1 {
				t.Fatalf("want one warning, got %d", len(recorder.Events))
			}
			if event := <-recorder.Events; !strings.HasPrefix(event, "Warning "+test.event+" ") {
				t.Fatalf("unexpected warning: %s", event)
			}
		})
	}
}

func TestVolumeAttributesClassProgressStall(t *testing.T) {
	now := metav1.Now()
	status := marklogicv1.PVCVolumeAttributesClassStatus{Name: "data-0", UID: "same", DesiredClassName: "premium", State: marklogicv1.VolumeAttributesClassPending, Reason: "VACPending", Message: "Waiting"}
	previous := status
	previous.LastProgressTime = metav1.NewTime(now.Add(-31 * time.Minute))
	preserveVolumeAttributesClassProgressTime(&status, previous, now)
	if status.State != marklogicv1.VolumeAttributesClassStalled || !status.LastProgressTime.Equal(&previous.LastProgressTime) {
		t.Fatal("unchanged pending work must become diagnostically stalled")
	}
	previous = status
	status.State = marklogicv1.VolumeAttributesClassPending
	preserveVolumeAttributesClassProgressTime(&status, previous, metav1.NewTime(now.Add(time.Minute)))
	if status.State != marklogicv1.VolumeAttributesClassStalled || !status.LastProgressTime.Equal(&previous.LastProgressTime) {
		t.Fatal("stalled observation must remain stable across retries")
	}
	previous = status
	status.State, status.Reason = marklogicv1.VolumeAttributesClassInProgress, "VACInProgress"
	preserveVolumeAttributesClassProgressTime(&status, previous, now)
	if status.State != marklogicv1.VolumeAttributesClassInProgress || !status.LastProgressTime.Equal(&now) {
		t.Fatal("native progress must clear a diagnostic stall")
	}
}

func TestVolumeAttributesClassClusterProgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*marklogicv1.MarklogicCluster, *marklogicv1.MarklogicGroup)
		state  marklogicv1.VolumeAttributesClassState
		reason string
	}{
		{name: "applied", state: marklogicv1.VolumeAttributesClassCompleted, reason: "VACApplied"},
		{name: "missing group", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			group.Name = "not-the-managed-group"
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildStatusMissing"},
		{name: "additional-only group", mutate: func(cluster *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			cluster.Spec.Persistence = &marklogicv1.Persistence{Enabled: false, Size: "20Gi"}
			cluster.Spec.AdditionalVolumeClaimTemplates = &[]corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "logs"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: pointerToVACStorageClass("logs-premium")}}}
			group.Spec.Persistence = cluster.Spec.Persistence.DeepCopy()
			group.Spec.AdditionalVolumeClaimTemplates = cluster.Spec.AdditionalVolumeClaimTemplates
			claim := &group.Status.Storage.VolumeAttributesClass.PVCs[0]
			claim.Name, claim.TemplateName, claim.DesiredClassName = "logs-dnode-0", "logs", "logs-premium"
			claim.RequestedClassName, claim.CurrentClassName = pointerToVACStorageClass("logs-premium"), pointerToVACStorageClass("logs-premium")
		}, state: marklogicv1.VolumeAttributesClassCompleted, reason: "VACApplied"},
		{name: "partial PVC evidence", mutate: func(cluster *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			replicas := int32(2)
			cluster.Spec.MarkLogicGroups[0].Replicas = &replicas
			group.Spec.Replicas = &replicas
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildStatusStale"},
		{name: "stale generation", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) { group.Generation++ }, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildStatusStale"},
		{name: "missing status", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) { group.Status.Storage = nil }, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildStatusMissing"},
		{name: "foreign child", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			group.OwnerReferences[0].UID = "foreign"
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildUnverified"},
		{name: "new cluster target", mutate: func(cluster *marklogicv1.MarklogicCluster, _ *marklogicv1.MarklogicGroup) {
			cluster.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("newest")
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildSpecNotConverged"},
		{name: "failed child", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			group.Status.Storage.VolumeAttributesClass.State = marklogicv1.VolumeAttributesClassFailed
			group.Status.Storage.VolumeAttributesClass.Reason = "VACInfeasible"
			group.Status.Conditions[0].Status = metav1.ConditionFalse
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "VACInfeasible"},
		{name: "missing PVC evidence", mutate: func(_ *marklogicv1.MarklogicCluster, group *marklogicv1.MarklogicGroup) {
			group.Status.Storage.VolumeAttributesClass.PVCs = nil
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "ChildStatusStale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, _, _, _ := liveVolumeAttributesClassFixture(t)
			cluster := &marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "testns", UID: "cluster-uid", Generation: 2}, Spec: marklogicv1.MarklogicClusterSpec{Persistence: storageWithClass("premium"), MarkLogicGroups: []*marklogicv1.MarklogicGroups{{Name: "dnode"}}}}
			group := operator.MarklogicGroup.DeepCopy()
			group.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cluster, marklogicv1.GroupVersion.WithKind("MarklogicCluster"))}
			group.Status.Storage = &marklogicv1.StorageStatus{VolumeAttributesClass: &marklogicv1.VolumeAttributesClassStatus{ObservedGeneration: group.Generation, State: marklogicv1.VolumeAttributesClassCompleted, Reason: "VACApplied", Message: "Applied", PVCs: []marklogicv1.PVCVolumeAttributesClassStatus{{Name: "datadir-dnode-0", TemplateName: "datadir", DesiredClassName: "premium", RequestedClassName: pointerToVACStorageClass("premium"), CurrentClassName: pointerToVACStorageClass("premium"), State: marklogicv1.VolumeAttributesClassCompleted}}}}
			group.Status.Conditions = []metav1.Condition{{Type: VolumeAttributesClassSyncedCondition, ObservedGeneration: group.Generation, Status: metav1.ConditionTrue, Reason: "VACApplied", LastTransitionTime: metav1.Now()}}
			if test.mutate != nil {
				test.mutate(cluster, group)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(operator.Scheme).WithStatusSubresource(cluster, group).WithObjects(cluster, group).Build()
			context := &ClusterContext{Ctx: operator.Ctx, Client: fakeClient, MarklogicCluster: cluster, Recorder: record.NewFakeRecorder(10)}
			if result, err := context.observeVolumeAttributesClasses(); err != nil || result.RequeueAfter != 30*time.Second {
				t.Fatalf("observe = %v, %v", result, err)
			}
			saved := &marklogicv1.MarklogicCluster{}
			if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(cluster), saved); err != nil {
				t.Fatal(err)
			}
			progress := saved.Status.Storage.VolumeAttributesClass
			if progress.State != test.state || progress.Reason != test.reason || progress.ObservedGeneration != cluster.Generation || len(progress.Groups) != 1 {
				t.Fatalf("unexpected cluster progress: %+v", progress)
			}
			before := saved.ResourceVersion
			if _, err := context.observeVolumeAttributesClasses(); err != nil {
				t.Fatal(err)
			}
			if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(cluster), saved); err != nil {
				t.Fatal(err)
			}
			if before != saved.ResourceVersion {
				t.Fatal("unchanged cluster aggregation must not write status")
			}
			cluster.Spec.Persistence = &marklogicv1.Persistence{Enabled: false, Size: "20Gi"}
			cluster.Spec.AdditionalVolumeClaimTemplates = nil
			cluster.ResourceVersion = saved.ResourceVersion
			if err := fakeClient.Update(operator.Ctx, cluster); err != nil {
				t.Fatal(err)
			}
			if _, err := context.observeVolumeAttributesClasses(); err != nil {
				t.Fatal(err)
			}
			if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(cluster), saved); err != nil {
				t.Fatal(err)
			}
			if saved.Status.Storage != nil || meta.FindStatusCondition(saved.Status.Conditions, VolumeAttributesClassSyncedCondition) != nil {
				t.Fatal("cluster opt-out must remove only the active VAC aggregate")
			}
		})
	}
}

func TestVolumeAttributesClassProgressEdgeCases(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OperatorContext, *corev1.PersistentVolumeClaim)
		state  marklogicv1.VolumeAttributesClassState
		reason string
	}{
		{name: "missing PVC", mutate: func(operator *OperatorContext, claim *corev1.PersistentVolumeClaim) {
			if err := operator.Client.Delete(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassPending, reason: "PVCNotFound"},
		{name: "no active targets", mutate: func(operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			replicas := int32(0)
			operator.MarklogicGroup.Spec.Replicas = &replicas
			if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "NoActivePVCs"},
		{name: "unsupported API", mutate: func(operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			operator.VACValidator = NewVolumeAttributesClassValidator(operator.APIReader, &vacDiscoveryStub{})
		}, state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACUnsupported"},
		{name: "missing class", mutate: func(operator *OperatorContext, _ *corev1.PersistentVolumeClaim) {
			if err := operator.Client.Delete(operator.Ctx, &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "premium"}}); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassFailed, reason: "VACNotFound"},
		{name: "provider message", mutate: func(operator *OperatorContext, claim *corev1.PersistentVolumeClaim) {
			claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
			if err := operator.Client.Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{TargetVolumeAttributesClassName: "premium", Status: "InProgress"}
			claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: "ModifyingVolume", Status: corev1.ConditionTrue}, {Type: "ModifyVolumeError", Status: corev1.ConditionTrue, Reason: "ProviderCooldown", Message: "retry after cooldown"}}
			if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
				t.Fatal(err)
			}
		}, state: marklogicv1.VolumeAttributesClassInProgress, reason: "ProviderCooldown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, _, _, claim := liveVolumeAttributesClassFixture(t)
			test.mutate(operator, claim)
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			status := saved.Status.Storage.VolumeAttributesClass
			if status.State != test.state || status.Reason != test.reason {
				t.Fatalf("status = %+v", status)
			}
			if test.reason == "ProviderCooldown" && !strings.Contains(status.PVCs[0].Message, "retry after cooldown") {
				t.Fatal("provider error details must be retained")
			}
		})
	}
}

func TestVolumeAttributesClassProgressStatusConflict(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(fmt.Sprintf("superseded=%t", superseded), func(t *testing.T) {
			operator, _, _, _ := liveVolumeAttributesClassFixture(t)
			attempts := 0
			operator.Client = interceptor.NewClient(operator.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, underlying client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
				attempts++
				if attempts == 1 {
					current := &marklogicv1.MarklogicGroup{}
					if err := underlying.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
						return err
					}
					if superseded {
						current.Generation++
						current.Spec.Persistence.VolumeAttributesClassName = pointerToVACStorageClass("newest")
						if err := underlying.Update(ctx, current); err != nil {
							return err
						}
					} else {
						current.Status.Stage = "ConcurrentUpdate"
						current.Status.Conditions = append(current.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()})
						if err := underlying.Status().Update(ctx, current); err != nil {
							return err
						}
					}
					return apierrors.NewConflict(schema.GroupResource{Group: marklogicv1.GroupVersion.Group, Resource: "marklogicgroups"}, object.GetName(), errors.New("concurrent update"))
				}
				return underlying.SubResource(subresource).Patch(ctx, object, patch, options...)
			}})
			if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
				t.Fatal(err)
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			if superseded {
				if attempts != 1 || saved.Status.Storage != nil {
					t.Fatal("superseded observations must not be persisted")
				}
			} else if attempts != 2 || saved.Status.Stage != "ConcurrentUpdate" || meta.FindStatusCondition(saved.Status.Conditions, "Ready") == nil || saved.Status.Storage == nil {
				t.Fatal("status conflict retry must preserve unrelated concurrent status")
			}
		})
	}
}

func TestVolumeAttributesClassProgressDispatchFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		state  marklogicv1.VolumeAttributesClassState
		reason string
	}{
		{name: "ambiguous timeout", err: apierrors.NewTimeoutError("response lost", 1), state: marklogicv1.VolumeAttributesClassUnknown, reason: "VACRequestUnconfirmed"},
		{name: "forbidden write", err: apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "datadir-dnode-0", errors.New("missing patch permission")), state: marklogicv1.VolumeAttributesClassFailed, reason: "VACRequestRejected"},
		{name: "dropped field", err: vacValidationError("VACFieldNotPersisted", "API did not retain the requested class", nil), state: marklogicv1.VolumeAttributesClassFailed, reason: "VACFieldNotPersisted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, _, _, claim := liveVolumeAttributesClassFixture(t)
			failure := errors.Join(&volumeAttributesClassClaimError{name: "another-claim", err: errors.New("independent target")}, &volumeAttributesClassClaimError{name: claim.Name, err: test.err})
			if _, err := operator.observeVolumeAttributesClasses(failure); err != nil {
				t.Fatal(err)
			}
			saved := &marklogicv1.MarklogicGroup{}
			if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
				t.Fatal(err)
			}
			if status := saved.Status.Storage.VolumeAttributesClass; status.State != test.state || status.Reason != test.reason {
				t.Fatalf("unexpected dispatch failure status: %+v", status)
			}
		})
	}
}

func TestVolumeAttributesClassProgressPrecedence(t *testing.T) {
	status := &marklogicv1.VolumeAttributesClassStatus{}
	for _, state := range []marklogicv1.VolumeAttributesClassState{marklogicv1.VolumeAttributesClassCompleted, marklogicv1.VolumeAttributesClassPending, marklogicv1.VolumeAttributesClassInProgress, marklogicv1.VolumeAttributesClassStalled, marklogicv1.VolumeAttributesClassUnknown, marklogicv1.VolumeAttributesClassFailed} {
		status.PVCs = append(status.PVCs, marklogicv1.PVCVolumeAttributesClassStatus{Name: string(state), State: state, Reason: string(state), Message: "detail"})
		summarizeVolumeAttributesClassProgress(status)
		if status.State != state {
			t.Fatalf("aggregate priority = %s, want %s", status.State, state)
		}
	}
	if len(status.PVCs) != 6 {
		t.Fatal("aggregation must preserve every partial PVC observation")
	}
}

func TestVolumeAttributesClassProgressConditionValidation(t *testing.T) {
	operator, _, _, claim := liveVolumeAttributesClassFixture(t)
	claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	reason, message := "provider failure (retryable)", strings.Repeat("provider detail ", 3000)
	claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: "ModifyVolumeError", Status: corev1.ConditionTrue, Reason: reason, Message: message}}
	if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
		t.Fatal(err)
	}
	saved := &marklogicv1.MarklogicGroup{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), saved); err != nil {
		t.Fatal(err)
	}
	progress := saved.Status.Storage.VolumeAttributesClass.PVCs[0]
	condition := meta.FindStatusCondition(saved.Status.Conditions, VolumeAttributesClassSyncedCondition)
	if progress.Reason != reason || progress.Message != message {
		t.Fatal("native provider diagnostics must remain intact in dedicated progress")
	}
	if condition.Reason != "VACInProgress" || len(condition.Message) > 32768 {
		t.Fatal("raw provider diagnostics must not produce an invalid Kubernetes Condition")
	}
}

func TestVolumeAttributesClassProgressIndependentTemplates(t *testing.T) {
	operator, statefulSet, template, primary := liveVolumeAttributesClassFixture(t)
	primary.Spec.VolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Update(operator.Ctx, primary); err != nil {
		t.Fatal(err)
	}
	primary.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("premium")
	if err := operator.Client.Status().Update(operator.Ctx, primary); err != nil {
		t.Fatal(err)
	}
	logs := template.DeepCopy()
	logs.Name, logs.Spec.VolumeAttributesClassName = "logs", pointerToVACStorageClass("logs-premium")
	operator.MarklogicGroup.Spec.AdditionalVolumeClaimTemplates = &[]corev1.PersistentVolumeClaim{*logs}
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, *logs)
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	claim := primary.DeepCopy()
	claim.Name, claim.UID, claim.ResourceVersion = "logs-dnode-0", "logs-uid", ""
	claim.Spec.VolumeName = "pv-logs"
	claim.Spec.VolumeAttributesClassName = pointerToVACStorageClass("logs-premium")
	claim.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("standard")
	volume := &corev1.PersistentVolume{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: primary.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	volume.Name, volume.UID, volume.ResourceVersion = "pv-logs", "pv-logs-uid", ""
	volume.Spec.ClaimRef.Name, volume.Spec.ClaimRef.UID = claim.Name, claim.UID
	profile := &storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "logs-premium"}, DriverName: "ebs.csi.aws.com", Parameters: map[string]string{"profile": "logs"}}
	for _, object := range []client.Object{claim, volume, profile} {
		if err := operator.Client.Create(operator.Ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	observe := func() *marklogicv1.MarklogicGroup {
		t.Helper()
		if _, err := operator.observeVolumeAttributesClasses(nil); err != nil {
			t.Fatal(err)
		}
		group := &marklogicv1.MarklogicGroup{}
		if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(operator.MarklogicGroup), group); err != nil {
			t.Fatal(err)
		}
		return group
	}
	group := observe()
	progress := group.Status.Storage.VolumeAttributesClass
	if len(progress.PVCs) != 2 || progress.PVCs[0].Name != primary.Name || progress.PVCs[0].State != marklogicv1.VolumeAttributesClassCompleted || progress.PVCs[1].Name != claim.Name || progress.PVCs[1].DesiredClassName != "logs-premium" || progress.State == marklogicv1.VolumeAttributesClassCompleted {
		t.Fatalf("independent partial progress was lost: %+v", progress)
	}
	group.Spec.Persistence.VolumeAttributesClassName = nil
	if err := operator.Client.Update(operator.Ctx, group); err != nil {
		t.Fatal(err)
	}
	operator.MarklogicGroup = group
	if progress := observe().Status.Storage.VolumeAttributesClass; len(progress.PVCs) != 1 || progress.PVCs[0].TemplateName != "logs" {
		t.Fatal("primary opt-out must not remove configured additional progress")
	}
	claim.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("logs-premium")
	if err := operator.Client.Status().Update(operator.Ctx, claim); err != nil {
		t.Fatal(err)
	}
	if progress := observe().Status.Storage.VolumeAttributesClass; progress.State != marklogicv1.VolumeAttributesClassCompleted {
		t.Fatal("additional-only group must complete when its own class is applied")
	}
}

func TestVolumeAttributesClassDefaultStorageClass(t *testing.T) {
	for _, test := range []struct {
		name      string
		noDefault bool
		tie       bool
		beta      bool
	}{
		{name: "newest default wins"},
		{name: "equal timestamps select alphabetical name", tie: true},
		{name: "legacy default annotation", beta: true},
		{name: "no default blocks validation", noDefault: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := storagev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			annotation := "storageclass.kubernetes.io/is-default-class"
			if test.beta {
				annotation = "storageclass.beta.kubernetes.io/is-default-class"
			}
			older, newer := metav1.NewTime(time.Unix(10, 0)), metav1.NewTime(time.Unix(20, 0))
			if test.tie {
				older = newer
			}
			annotations := map[string]string{annotation: "true"}
			if test.noDefault {
				annotations = nil
			}
			class := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttributesClass", "metadata": map[string]interface{}{"name": "profile"}, "driverName": "ebs.csi.aws.com",
			}}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class,
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "z-old", CreationTimestamp: older, Annotations: annotations}, Provisioner: "other.csi.example.com"},
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "a-new", CreationTimestamp: newer, Annotations: annotations}, Provisioner: "ebs.csi.aws.com"},
			).Build()
			validator := NewVolumeAttributesClassValidator(reader, vacDiscoveryFor("v1"))
			template := generatePVCTemplate(storageWithClass("profile"))
			template.Spec.StorageClassName = nil
			original := template.DeepCopy()
			reason := ""
			if test.noDefault {
				reason = "VACStorageClassUnavailable"
			}
			assertVACValidationReason(t, validator.Validate(context.Background(), []corev1.PersistentVolumeClaim{template}), reason)
			if !reflect.DeepEqual(&template, original) {
				t.Fatal("validation must not rewrite the template or pin the default StorageClass")
			}
		})
	}
}

func vacValidatorForGroup(t *testing.T, group *marklogicv1.MarklogicGroup) *VolumeAttributesClassValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parameters := generateStatefulSetsParams(group)
	templates := []corev1.PersistentVolumeClaim{}
	if parameters.AdditionalVolumeClaimTemplates != nil {
		templates = append(templates, (*parameters.AdditionalVolumeClaimTemplates)...)
	}
	if parameters.PersistentVolumeClaim.Name != "" {
		templates = append(templates, parameters.PersistentVolumeClaim)
	}
	objects := map[string]client.Object{}
	for _, template := range templates {
		if className := template.Spec.VolumeAttributesClassName; className != nil && *className != "" {
			objects["vac/"+*className] = &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttributesClass",
				"metadata": map[string]interface{}{"name": *className}, "driverName": "ebs.csi.aws.com",
			}}
		}
		name := "gp3"
		if template.Spec.StorageClassName != nil {
			name = *template.Spec.StorageClassName
		}
		objects["sc/"+name] = &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "ebs.csi.aws.com"}
	}
	values := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		values = append(values, object)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(values...).Build()
	return NewVolumeAttributesClassValidator(reader, vacDiscoveryFor("v1"))
}

func storageWithClass(className string) *marklogicv1.Persistence {
	return &marklogicv1.Persistence{Enabled: true, Size: "20Gi", StorageClassName: "gp3", VolumeAttributesClassName: &className}
}

func TestDay1VolumeAttributesClassProvisioning(t *testing.T) {
	tests := []struct {
		name             string
		clusterStorage   *marklogicv1.Persistence
		groupPersistence *marklogicv1.Persistence
		wantClass        string
	}{
		{name: "unconfigured"},
		{name: "empty cluster class", clusterStorage: storageWithClass("")},
		{name: "cluster inheritance", clusterStorage: storageWithClass("ml-standard"), wantClass: "ml-standard"},
		{name: "group override", clusterStorage: storageWithClass("ml-standard"), groupPersistence: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "group only", groupPersistence: storageWithClass("ml-premium"), wantClass: "ml-premium"},
		{name: "explicit opt out", clusterStorage: storageWithClass("ml-standard"), groupPersistence: storageWithClass("")},
		{
			name:             "persistence override does not inherit class",
			clusterStorage:   storageWithClass("ml-standard"),
			groupPersistence: &marklogicv1.Persistence{Enabled: true, Size: "50Gi", StorageClassName: "other"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			replicas := int32(3)
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: "testns"},
				Spec: marklogicv1.MarklogicClusterSpec{
					Image:         "marklogic:12.0.3",
					ClusterDomain: "cluster.local",
					Persistence:   &marklogicv1.Persistence{Enabled: true, Size: "20Gi", StorageClassName: "gp3"},
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{
						{Name: "dnode", Replicas: &replicas, IsBootstrap: true, GroupConfig: &marklogicv1.GroupConfig{Name: "Default"}},
						{Name: "enode", Replicas: &replicas, Persistence: test.groupPersistence, GroupConfig: &marklogicv1.GroupConfig{Name: "query"}},
					},
				},
			}
			if test.clusterStorage != nil {
				cluster.Spec.Persistence = test.clusterStorage
			}
			context := &ClusterContext{MarklogicCluster: cluster}
			clusterParams := generateMarkLogicClusterParams(cluster)
			for index := range cluster.Spec.MarkLogicGroups {
				params := generateMarkLogicGroupParams(cluster, index, clusterParams)
				group := context.GenerateMarkLogicGroupDef(cluster, index, params)
				wantClass := test.wantClass
				if index == 0 {
					wantClass = ""
					if test.clusterStorage != nil && test.clusterStorage.VolumeAttributesClassName != nil {
						wantClass = *test.clusterStorage.VolumeAttributesClassName
					}
				}
				claim := generateStatefulSetsParams(group).PersistentVolumeClaim
				if claim.Name != "datadir" {
					t.Fatalf("expected primary datadir claim, got %q", claim.Name)
				}
				assertPVCClass(t, claim, wantClass)
				if index == 1 && test.groupPersistence != nil && claim.Spec.Resources.Requests.Storage().String() != test.groupPersistence.Size {
					t.Fatal("VAC selection must preserve the complete group persistence override")
				}
				if group.Spec.Persistence.VolumeAttributesClassName != nil && wantClass != "" {
					*group.Spec.Persistence.VolumeAttributesClassName = "changed-after-generation"
					assertPVCClass(t, claim, wantClass)
					if *params.Persistence.VolumeAttributesClassName != wantClass {
						t.Fatal("generated child storage must not share its class pointer with parameters")
					}
				}
			}
		})
	}
}

func assertPVCClass(t *testing.T, claim corev1.PersistentVolumeClaim, want string) {
	t.Helper()
	if want == "" {
		if claim.Spec.VolumeAttributesClassName != nil {
			t.Fatalf("expected no class reference, got %q", *claim.Spec.VolumeAttributesClassName)
		}
	} else if claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != want {
		t.Fatalf("expected class %q, got %v", want, claim.Spec.VolumeAttributesClassName)
	}
	encoded, err := json.Marshal(claim.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	_, present := fields["volumeAttributesClassName"]
	if present != (want != "") {
		t.Fatalf("unexpected serialized class field: %s", encoded)
	}
}

func TestDay1StorageStandaloneAndTemplateIsolation(t *testing.T) {
	replicas := int32(2)
	auxiliary := []corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "logsdir"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fluent-bit"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: storageWithClass("logs-profile").VolumeAttributesClassName}},
	}
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns"},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:                           "dnode",
			Replicas:                       &replicas,
			Image:                          "marklogic:12.0.3",
			Persistence:                    &marklogicv1.Persistence{Enabled: true, Size: "20Gi"},
			GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"},
			HugePages:                      &marklogicv1.HugePages{},
			LogCollection:                  &marklogicv1.LogCollection{},
			AdditionalVolumeClaimTemplates: &auxiliary,
		},
	}
	baseline := generateStatefulSetsDef(group.ObjectMeta, generateStatefulSetsParams(group), metav1.OwnerReference{}, generateContainerParams(group))
	group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-standard").VolumeAttributesClassName
	configured := generateStatefulSetsDef(group.ObjectMeta, generateStatefulSetsParams(group), metav1.OwnerReference{}, generateContainerParams(group))
	assertPVCClass(t, configured.Spec.VolumeClaimTemplates[0], "ml-standard")
	if !reflect.DeepEqual(configured.Spec.VolumeClaimTemplates[1:], auxiliary) {
		t.Fatal("VAC must not alter auxiliary or Fluent Bit claim templates")
	}
	if !reflect.DeepEqual(baseline.Spec.Template, configured.Spec.Template) {
		t.Fatal("VAC must not change the Pod template")
	}
	for _, persistence := range []*marklogicv1.Persistence{nil, {Enabled: false, Size: "20Gi"}} {
		group.Spec.Persistence = persistence
		params := generateStatefulSetsParams(group)
		if params.PersistentVolumeClaim.Spec.VolumeAttributesClassName != nil {
			t.Fatal("disabled persistence must not receive a VAC")
		}
		statefulSet := generateStatefulSetsDef(group.ObjectMeta, params, metav1.OwnerReference{}, generateContainerParams(group))
		if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, auxiliary) {
			t.Fatal("disabled persistence must not create a primary data claim")
		}
	}
}

func TestDay1VACStatefulSetReconciliation(t *testing.T) {
	tests := []struct {
		name           string
		liveStorage    *marklogicv1.Persistence
		desiredStorage *marklogicv1.Persistence
		create         bool
	}{
		{name: "create with class", desiredStorage: storageWithClass("ml-standard"), create: true},
		{name: "create without class", create: true},
		{name: "create opted out", desiredStorage: storageWithClass(""), create: true},
		{name: "changed class", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("ml-premium")},
		{name: "newly selected class", desiredStorage: storageWithClass("ml-standard")},
		{name: "removed storage", liveStorage: storageWithClass("ml-standard")},
		{name: "explicit opt out", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("")},
		{name: "unchanged class", liveStorage: storageWithClass("ml-standard"), desiredStorage: storageWithClass("ml-standard")},
		{name: "unconfigured"},
	}
	for _, test := range tests {
		for _, desiredReplicas := range []int32{1, 3} {
			t.Run(fmt.Sprintf("%s/replicas-%d", test.name, desiredReplicas), func(t *testing.T) {
				scheme := runtime.NewScheme()
				for _, addToScheme := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
					if err := addToScheme(scheme); err != nil {
						t.Fatal(err)
					}
				}
				liveReplicas := int32(1)
				auxiliary := []corev1.PersistentVolumeClaim{
					{ObjectMeta: metav1.ObjectMeta{Name: "logsdir"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: storageWithClass("logs-profile").VolumeAttributesClassName}},
				}
				group := &marklogicv1.MarklogicGroup{
					ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "dnode-uid"},
					Spec: marklogicv1.MarklogicGroupSpec{
						Name:                           "dnode",
						Replicas:                       &liveReplicas,
						Image:                          "marklogic:12.0.3",
						Persistence:                    &marklogicv1.Persistence{Enabled: true, Size: "20Gi"},
						GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"},
						HugePages:                      &marklogicv1.HugePages{},
						LogCollection:                  &marklogicv1.LogCollection{},
						AdditionalVolumeClaimTemplates: &auxiliary,
					},
				}
				if test.liveStorage != nil {
					group.Spec.Persistence.VolumeAttributesClassName = test.liveStorage.VolumeAttributesClassName
				}
				labels := getSelectorLabelsByComponent(group.Spec.Name, group.Spec.IsDynamic)
				labels["app.kubernetes.io/instance"] = group.Spec.Name
				labels["app.kubernetes.io/component"] = getMarkLogicComponentLabel(group.Spec.IsDynamic)
				current := generateStatefulSetsDef(
					generateObjectMeta(group.Spec.Name, group.Namespace, labels, nil),
					generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
				).DeepCopy()
				group.Spec.Persistence.VolumeAttributesClassName = nil
				if test.desiredStorage != nil {
					group.Spec.Persistence.VolumeAttributesClassName = test.desiredStorage.VolumeAttributesClassName
				}
				group.Spec.Replicas = &desiredReplicas
				newClaimTemplate := generateStatefulSetsParams(group).PersistentVolumeClaim
				precreate := !test.create && !reflect.DeepEqual(newClaimTemplate.Spec.VolumeAttributesClassName, current.Spec.VolumeClaimTemplates[0].Spec.VolumeAttributesClassName)
				objects := []client.Object{group}
				if !test.create {
					objects = append(objects, current)
				}
				fakeClient := fake.NewClientBuilder().
					WithScheme(scheme).
					WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
					WithObjects(objects...).
					WithInterceptorFuncs(interceptor.Funcs{
						Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
							if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
								if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
									return fmt.Errorf("StatefulSet volumeClaimTemplates are immutable")
								}
								if precreate {
									for ordinal := liveReplicas; ordinal < *statefulSet.Spec.Replicas; ordinal++ {
										claim := &corev1.PersistentVolumeClaim{}
										key := client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}
										if err := underlying.Get(ctx, key, claim); err != nil {
											return fmt.Errorf("PVC must exist before scale-up: %w", err)
										}
										if !reflect.DeepEqual(claim.Spec, newClaimTemplate.Spec) {
											return fmt.Errorf("scale-up PVC does not use the current persistence and VAC settings")
										}
									}
								}
							}
							return underlying.Patch(ctx, object, patch, options...)
						},
					}).Build()
				operatorContext := &OperatorContext{
					Ctx: context.Background(), Client: fakeClient, Scheme: scheme,
					MarklogicGroup: group, Recorder: record.NewFakeRecorder(10),
					VACValidator: vacValidatorForGroup(t, group),
				}
				if _, err := operatorContext.ReconcileStatefulset(); err != nil {
					t.Fatal(err)
				}
				actual := &appsv1.StatefulSet{}
				if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
					t.Fatal(err)
				}
				wantStorage := test.liveStorage
				if test.create {
					wantStorage = test.desiredStorage
				}
				wantClass := ""
				if wantStorage != nil && wantStorage.VolumeAttributesClassName != nil {
					wantClass = *wantStorage.VolumeAttributesClassName
				}
				assertPVCClass(t, actual.Spec.VolumeClaimTemplates[0], wantClass)
				if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates[1:], auxiliary) {
					t.Fatal("reconciliation must not alter auxiliary claim templates")
				}
				if actual.Spec.Replicas == nil || *actual.Spec.Replicas != desiredReplicas {
					t.Fatalf("expected %d replicas, got %v", desiredReplicas, actual.Spec.Replicas)
				}
				claims := &corev1.PersistentVolumeClaimList{}
				if err := fakeClient.List(operatorContext.Ctx, claims, client.InNamespace(group.Namespace)); err != nil {
					t.Fatal(err)
				}
				wantClaims := 0
				if precreate {
					wantClaims = int(desiredReplicas - liveReplicas)
				}
				if len(claims.Items) != wantClaims {
					t.Fatalf("expected %d precreated data PVCs, got %d", wantClaims, len(claims.Items))
				}
				for _, claim := range claims.Items {
					if !reflect.DeepEqual(claim.Spec, newClaimTemplate.Spec) {
						t.Fatal("new data PVC must use the current persistence and VAC settings")
					}
				}
			})
		}
	}
}

func TestAdditionalVolumeAttributesClassInheritance(t *testing.T) {
	clusterTemplates := []corev1.PersistentVolumeClaim{generatePVCTemplate(storageWithClass("logs-standard")), generatePVCTemplate(storageWithClass("backup-standard"))}
	clusterTemplates[0].Name, clusterTemplates[1].Name = "logsdir", "backup"
	override := []corev1.PersistentVolumeClaim{*clusterTemplates[0].DeepCopy()}
	override[0].Spec.VolumeAttributesClassName = storageWithClass("logs-premium").VolumeAttributesClassName
	empty := []corev1.PersistentVolumeClaim{}
	for _, test := range []struct {
		name     string
		override *[]corev1.PersistentVolumeClaim
		want     []corev1.PersistentVolumeClaim
	}{
		{name: "inherit complete cluster list", want: clusterTemplates},
		{name: "replace rather than merge list", override: &override, want: override},
		{name: "explicit empty list", override: &empty, want: empty},
	} {
		t.Run(test.name, func(t *testing.T) {
			replicas := int32(2)
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: "testns"},
				Spec: marklogicv1.MarklogicClusterSpec{
					Persistence: storageWithClass("primary-standard"), AdditionalVolumeClaimTemplates: &clusterTemplates,
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{{
						Name: "dnode", IsBootstrap: true, Replicas: &replicas,
						Persistence: &marklogicv1.Persistence{Enabled: false, Size: "20Gi"}, AdditionalVolumeClaimTemplates: test.override,
					}},
				},
			}
			context := &ClusterContext{MarklogicCluster: cluster}
			parameters := generateMarkLogicGroupParams(cluster, 0, generateMarkLogicClusterParams(cluster))
			group := context.GenerateMarkLogicGroupDef(cluster, 0, parameters)
			actual := generateStatefulSetsParams(group)
			if !reflect.DeepEqual(*actual.AdditionalVolumeClaimTemplates, test.want) {
				t.Fatal("additional profiles must follow complete list inheritance/override independently of persistence")
			}
			if actual.PersistentVolumeClaim.Name != "" {
				t.Fatal("disabled primary persistence must not create datadir")
			}
			if len(test.want) > 0 {
				*(*actual.AdditionalVolumeClaimTemplates)[0].Spec.VolumeAttributesClassName = "mutated"
				if *test.want[0].Spec.VolumeAttributesClassName == "mutated" {
					t.Fatal("generated additional templates must not share class pointers with the CR")
				}
			}
		})
	}
}

func TestAdditionalVolumeAttributesClassReconciliation(t *testing.T) {
	for _, test := range []struct {
		name           string
		create         bool
		replicas       int32
		disablePrimary bool
		retained       bool
		failCreate     bool
		optOut         bool
		changePrimary  bool
		partialCreate  bool
	}{
		{name: "initial independent profiles", create: true, replicas: 3},
		{name: "existing immutable templates", replicas: 1},
		{name: "scale multiple profiles", replicas: 3},
		{name: "scale primary and additional profiles", replicas: 3, changePrimary: true},
		{name: "additional volumes without primary persistence", replicas: 3, disablePrimary: true},
		{name: "reuse retained additional claim", replicas: 3, retained: true},
		{name: "additional creation failure blocks replicas", replicas: 3, failCreate: true},
		{name: "partial additional creation blocks replicas", replicas: 3, partialCreate: true},
		{name: "additional opt out", replicas: 3, optOut: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
				if err := register(scheme); err != nil {
					t.Fatal(err)
				}
			}
			liveReplicas := int32(1)
			additional := []corev1.PersistentVolumeClaim{generatePVCTemplate(storageWithClass("logs-standard")), generatePVCTemplate(storageWithClass(""))}
			additional[0].Name, additional[1].Name = "logsdir", "backup"
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid"},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &liveReplicas, Persistence: storageWithClass("ml-standard"),
					AdditionalVolumeClaimTemplates: &additional,
					GroupConfig:                    &marklogicv1.GroupConfig{Name: "Default"}, HugePages: &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				},
			}
			group.Spec.Persistence.Enabled = !test.disablePrimary
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabels(group.Spec.Name), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			).DeepCopy()
			current.UID = "statefulset-uid"
			objects := []client.Object{group}
			if !test.create {
				objects = append(objects, current)
			}
			var retained *corev1.PersistentVolumeClaim
			if test.retained {
				retained = additional[0].DeepCopy()
				retained.Name, retained.Namespace = "logsdir-dnode-1", group.Namespace
				retained.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(group.UID)}
				objects = append(objects, retained)
			}
			additional[0].Spec.VolumeAttributesClassName = storageWithClass("logs-premium").VolumeAttributesClassName
			additional[1].Spec.VolumeAttributesClassName = storageWithClass("backup-profile").VolumeAttributesClassName
			if test.optOut {
				additional[0].Spec.VolumeAttributesClassName = storageWithClass("").VolumeAttributesClassName
			}
			if test.changePrimary {
				group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-premium").VolumeAttributesClassName
			}
			group.Spec.Replicas = &test.replicas
			group.Spec.Image = "marklogic:12.0.4"
			group.Spec.NodeSelector = map[string]string{"nodepool": "updated"}
			desired := generateStatefulSetsParams(group)
			expectedTemplates := append([]corev1.PersistentVolumeClaim{}, (*desired.AdditionalVolumeClaimTemplates)...)
			if test.changePrimary {
				expectedTemplates = append(expectedTemplates, desired.PersistentVolumeClaim)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicGroup{}).WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if _, claim := object.(*corev1.PersistentVolumeClaim); claim && (test.failCreate || test.partialCreate && object.GetName() == "backup-dnode-1") {
							return fmt.Errorf("additional PVC creation denied")
						}
						return underlying.Create(ctx, object, options...)
					},
					Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
							if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
								return fmt.Errorf("claim templates are immutable")
							}
							for _, template := range expectedTemplates {
								for ordinal := liveReplicas; ordinal < *statefulSet.Spec.Replicas; ordinal++ {
									claim := &corev1.PersistentVolumeClaim{}
									if err := underlying.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("%s-dnode-%d", template.Name, ordinal)}, claim); err != nil {
										return fmt.Errorf("all additional claims must exist before scaling: %w", err)
									}
								}
							}
						}
						return underlying.Patch(ctx, object, patch, options...)
					},
				}).Build()
			operator := &OperatorContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: record.NewFakeRecorder(10)}
			operator.VACValidator = vacValidatorForGroup(t, group)
			outcome, err := operator.ReconcileStatefulset()
			if err != nil {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			if (test.failCreate || test.partialCreate) && outcome.RequeueAfter <= 0 {
				t.Fatal("failed additional PVC preparation must request a retry")
			}
			actual := &appsv1.StatefulSet{}
			if err := fakeClient.Get(operator.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			wantReplicas := test.replicas
			if test.failCreate || test.partialCreate {
				wantReplicas = liveReplicas
			}
			if *actual.Spec.Replicas != wantReplicas {
				t.Fatal("PVC preparation must precede replica updates")
			}
			if actual.Spec.Template.Spec.Containers[0].Image != group.Spec.Image || !reflect.DeepEqual(actual.Spec.Template.Spec.NodeSelector, group.Spec.NodeSelector) {
				t.Fatal("additional PVC preparation must not block image and scheduling updates")
			}
			if !test.create && !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
				t.Fatal("existing primary and additional templates must remain unchanged")
			}
			if test.create {
				for index, template := range *desired.AdditionalVolumeClaimTemplates {
					if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates[len(actual.Spec.VolumeClaimTemplates)-2+index].Spec, template.Spec) {
						t.Fatal("initial additional PVCs must use independently configured classes")
					}
				}
			}
			claims := &corev1.PersistentVolumeClaimList{}
			if err := fakeClient.List(operator.Ctx, claims); err != nil {
				t.Fatal(err)
			}
			for _, claim := range claims.Items {
				if retained != nil && claim.Name == retained.Name {
					if !reflect.DeepEqual(claim.Spec, retained.Spec) {
						t.Fatal("retained additional claim must not be retargeted")
					}
					continue
				}
				matched := false
				for _, template := range expectedTemplates {
					for ordinal := liveReplicas; ordinal < test.replicas; ordinal++ {
						if claim.Name == fmt.Sprintf("%s-dnode-%d", template.Name, ordinal) {
							matched = reflect.DeepEqual(claim.Spec, template.Spec)
						}
					}
				}
				if !matched {
					t.Fatal("each additional scale-up PVC must use only its own template profile")
				}
			}
			if test.optOut && *additional[0].Spec.VolumeAttributesClassName != "" {
				t.Fatal("normalizing an additional opt-out must not mutate the CR")
			}
		})
	}
}

func TestScaleUpPVCCompatibility(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*corev1.PersistentVolumeClaim)
		wantError bool
	}{
		{name: "matching settings"},
		{name: "explicit filesystem default", mutate: func(claim *corev1.PersistentVolumeClaim) {
			mode := corev1.PersistentVolumeFilesystem
			claim.Spec.VolumeMode = &mode
		}},
		{name: "block versus filesystem", wantError: true, mutate: func(claim *corev1.PersistentVolumeClaim) {
			mode := corev1.PersistentVolumeBlock
			claim.Spec.VolumeMode = &mode
		}},
		{name: "incompatible access mode", wantError: true, mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}
		}},
		{name: "undersized request", wantError: true, mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1Gi")
		}},
		{name: "expansion not yet applied", wantError: true, mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Phase = corev1.ClaimBound
			claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
		}},
		{name: "larger bound volume", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Status.Phase = corev1.ClaimBound
			claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator, set, template, _ := liveVolumeAttributesClassFixture(t)
			template.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			claim := template.DeepCopy()
			claim.Name, claim.Namespace, claim.UID = "datadir-dnode-1", operator.MarklogicGroup.Namespace, "retained-uid"
			claim.Annotations = map[string]string{dataPVCGroupUIDAnnotation: string(operator.MarklogicGroup.UID)}
			if test.mutate != nil {
				test.mutate(claim)
			}
			before := claim.DeepCopy()
			if err := verifyScaleUpPVC(operator.MarklogicGroup, set, &template, claim); (err != nil) != test.wantError {
				t.Fatalf("compatibility error=%v, wantError=%t", err, test.wantError)
			}
			if !reflect.DeepEqual(before, claim) {
				t.Fatal("compatibility checks must not mutate retained claims")
			}
		})
	}
}

func TestScaleUpReusesOrdinaryClaimAfterVACProvenanceIsRecorded(t *testing.T) {
	operator, statefulSet, desiredTemplate, activeClaim := liveVolumeAttributesClassFixture(t)
	replicas := int32(2)
	operator.MarklogicGroup.Spec.Replicas = &replicas
	if err := operator.Client.Update(operator.Ctx, operator.MarklogicGroup); err != nil {
		t.Fatal(err)
	}
	statefulSet.Spec.Replicas = &replicas
	statefulSet.Spec.Selector = &metav1.LabelSelector{MatchLabels: getSelectorLabels(operator.MarklogicGroup.Name)}
	statefulSet.Spec.VolumeClaimTemplates[0].Spec.VolumeAttributesClassName = pointerToVACStorageClass("standard")
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}

	retained := activeClaim.DeepCopy()
	retained.Name = "datadir-dnode-1"
	retained.UID = "ordinary-retained-uid"
	retained.ResourceVersion = ""
	retained.Annotations = nil
	retained.Spec.VolumeName = "pv-retained"
	retained.Status.CurrentVolumeAttributesClassName = pointerToVACStorageClass("standard")
	if err := operator.Client.Create(operator.Ctx, retained); err != nil {
		t.Fatal(err)
	}
	volume := &corev1.PersistentVolume{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKey{Name: activeClaim.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	volume.Name = retained.Spec.VolumeName
	volume.UID = "retained-volume-uid"
	volume.ResourceVersion = ""
	volume.Spec.ClaimRef.Name = retained.Name
	volume.Spec.ClaimRef.UID = retained.UID
	if err := operator.Client.Create(operator.Ctx, volume); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dnode-1", Namespace: retained.Namespace, UID: "retained-pod-uid",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
		},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "datadir", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: retained.Name}}}}},
	}
	if err := operator.Client.Create(operator.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	if submitted, err := operator.reconcileBoundVolumeAttributesClass(statefulSet, desiredTemplate, 1, retained); err != nil || !submitted {
		t.Fatalf("VAC update for ordinary claim: submitted=%v, err=%v", submitted, err)
	}
	saved := &corev1.PersistentVolumeClaim{}
	if err := operator.Client.Get(operator.Ctx, client.ObjectKeyFromObject(retained), saved); err != nil {
		t.Fatal(err)
	}
	if saved.Annotations[dataPVCGroupUIDAnnotation] != string(operator.MarklogicGroup.UID) {
		t.Fatal("verified ordinary claim must retain its group provenance")
	}

	if err := operator.Client.Delete(operator.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	oneReplica := int32(1)
	statefulSet.Spec.Replicas = &oneReplica
	if err := operator.Client.Update(operator.Ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	if err := operator.ensureScaleUpDataPVCs(operator.MarklogicGroup, statefulSet, []corev1.PersistentVolumeClaim{desiredTemplate}); err != nil {
		t.Fatalf("scale-up should reuse the retained ordinary claim after its Pod is gone: %v", err)
	}
}

func TestScaleUpVACPrecreationSafety(t *testing.T) {
	tests := []struct {
		name           string
		start          int32
		existing       bool
		unverified     bool
		foreignOwner   bool
		wrongClass     bool
		terminating    bool
		failCreate     bool
		partialCreate  bool
		alreadyExists  bool
		failUpdateOnce bool
		unowned        bool
		wantError      bool
		wantBlocked    bool
		invalidVAC     bool
		failRead       bool
		terminatingSet bool
		ready          bool
	}{
		{name: "nonzero starting ordinal", start: 5},
		{name: "retained claim is unchanged", existing: true},
		{name: "unverified retained claim blocks scaling", existing: true, unverified: true, wantBlocked: true},
		{name: "foreign owner blocks scaling", existing: true, foreignOwner: true, wantBlocked: true},
		{name: "incompatible retained claim blocks scaling", existing: true, wrongClass: true, wantBlocked: true},
		{name: "terminating retained claim blocks scaling", existing: true, terminating: true, wantBlocked: true},
		{name: "create failure blocks scaling", failCreate: true, wantBlocked: true},
		{name: "ready workload creation failure preserves updates", ready: true, failCreate: true, wantBlocked: true},
		{name: "retry after partial PVC creation", partialCreate: true},
		{name: "already exists race is reread", alreadyExists: true},
		{name: "retry after replica update failure", failUpdateOnce: true},
		{name: "unowned StatefulSet blocks precreation", unowned: true, wantError: true},
		{name: "invalid VAC retained verification failure preserves updates", invalidVAC: true, existing: true, wrongClass: true, wantBlocked: true},
		{name: "invalid VAC foreign retained owner preserves updates", invalidVAC: true, existing: true, foreignOwner: true, wantBlocked: true},
		{name: "invalid VAC PVC read failure preserves updates", invalidVAC: true, failRead: true, wantBlocked: true},
		{name: "invalid VAC unowned StatefulSet blocks updates", invalidVAC: true, unowned: true, wantError: true},
		{name: "terminating StatefulSet blocks updates", terminatingSet: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, addToScheme := range []func(*runtime.Scheme) error{marklogicv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
				if err := addToScheme(scheme); err != nil {
					t.Fatal(err)
				}
			}
			liveReplicas, desiredReplicas := int32(1), int32(3)
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns", UID: "group-uid"},
				Spec: marklogicv1.MarklogicGroupSpec{
					Name: "dnode", Replicas: &liveReplicas, Image: "marklogic:12.0.3",
					Persistence: &marklogicv1.Persistence{
						Enabled: true, Size: "20Gi", StorageClassName: "gp3",
						VolumeAttributesClassName: storageWithClass("ml-standard").VolumeAttributesClassName,
						AccessModes:               []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Annotations:               map[string]string{"storage-test": "preserved"},
					},
					GroupConfig: &marklogicv1.GroupConfig{Name: "Default"},
					HugePages:   &marklogicv1.HugePages{}, LogCollection: &marklogicv1.LogCollection{},
				},
			}
			current := generateStatefulSetsDef(
				generateObjectMeta(group.Spec.Name, group.Namespace, getSelectorLabels(group.Spec.Name), nil),
				generateStatefulSetsParams(group), marklogicServerAsOwner(group), generateContainerParams(group),
			).DeepCopy()
			current.UID = "statefulset-uid"
			if test.unowned {
				current.OwnerReferences = nil
			}
			if test.terminatingSet {
				now := metav1.Now()
				current.DeletionTimestamp = &now
				current.Finalizers = []string{"storage-test"}
			}
			current.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: test.start}
			current.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
			if test.ready {
				current.Status.Replicas = liveReplicas
				current.Status.ReadyReplicas = liveReplicas
			}
			group.Spec.Persistence.VolumeAttributesClassName = storageWithClass("ml-premium").VolumeAttributesClassName
			group.Spec.Replicas = &desiredReplicas
			group.Spec.Image = "marklogic:12.0.4"
			group.Spec.NodeSelector = map[string]string{"nodepool": "updated"}
			newTemplate := generateStatefulSetsParams(group).PersistentVolumeClaim
			objects := []client.Object{group, current}
			active := current.Spec.VolumeClaimTemplates[0].DeepCopy()
			active.Name = fmt.Sprintf("datadir-%s-%d", current.Name, test.start)
			active.Namespace = group.Namespace
			objects = append(objects, active)
			var retained *corev1.PersistentVolumeClaim
			if test.existing {
				retained = active.DeepCopy()
				retained.Name = fmt.Sprintf("datadir-%s-%d", current.Name, test.start+1)
				retained.Annotations[dataPVCGroupUIDAnnotation] = string(group.UID)
				retained.Status.Phase = corev1.ClaimPending
				if test.unverified {
					delete(retained.Annotations, dataPVCGroupUIDAnnotation)
				}
				if test.foreignOwner {
					retained.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", UID: "unrelated-statefulset"}}
				}
				if test.wrongClass {
					className := "unrelated-storage-class"
					retained.Spec.StorageClassName = &className
				}
				if test.terminating {
					now := metav1.Now()
					retained.DeletionTimestamp = &now
					retained.Finalizers = []string{"kubernetes.io/pvc-protection"}
				}
				objects = append(objects, retained)
			}
			failedUpdate := false
			createCalls := 0
			patchCalls := 0
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
				WithIndex(&corev1.PersistentVolumeClaim{}, PrecreatedPVCGroupUIDIndex, PrecreatedPVCGroupUIDs).
				WithIndex(&corev1.Pod{}, PodPVCReferenceIndex, PodPVCReferences).
				WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, claim := object.(*corev1.PersistentVolumeClaim); claim && test.failRead && key.Name == "datadir-dnode-1" {
							return apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, key.Name, fmt.Errorf("PVC reads denied"))
						}
						return underlying.Get(ctx, key, object, options...)
					},
					Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
							createCalls++
							if test.failCreate || test.partialCreate && createCalls == 2 {
								return apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName(), fmt.Errorf("PVC creation denied"))
							}
							object.SetUID(types.UID("precreated-" + object.GetName()))
							if test.alreadyExists {
								if err := underlying.Create(ctx, object, options...); err != nil {
									return err
								}
								return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName())
							}
						}
						return underlying.Create(ctx, object, options...)
					},
					Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
							patchCalls++
							if !reflect.DeepEqual(statefulSet.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) {
								return fmt.Errorf("StatefulSet volumeClaimTemplates are immutable")
							}
							if test.failUpdateOnce && !failedUpdate {
								failedUpdate = true
								return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, current.Name, fmt.Errorf("replica update conflict"))
							}
							for ordinal := test.start + liveReplicas; ordinal < test.start+*statefulSet.Spec.Replicas; ordinal++ {
								claim := &corev1.PersistentVolumeClaim{}
								if err := underlying.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}, claim); err != nil {
									return fmt.Errorf("PVC must exist before scale-up: %w", err)
								}
							}
						}
						return underlying.Patch(ctx, object, patch, options...)
					},
				}).Build()
			recorder := record.NewFakeRecorder(20)
			operatorContext := &OperatorContext{Ctx: context.Background(), Client: fakeClient, Scheme: scheme, MarklogicGroup: group, Recorder: recorder}
			operatorContext.VACValidator = vacValidatorForGroup(t, group)
			if test.invalidVAC {
				storageScheme := runtime.NewScheme()
				if err := storagev1.AddToScheme(storageScheme); err != nil {
					t.Fatal(err)
				}
				operatorContext.VACValidator.reader = fake.NewClientBuilder().WithScheme(storageScheme).WithObjects(
					&storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{Name: "ml-premium"}, DriverName: "different.csi.example.com"},
					&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3"}, Provisioner: "ebs.csi.aws.com"},
				).Build()
			}
			assertUnused := func(want int) {
				t.Helper()
				if _, err := operatorContext.observeUnusedPrecreatedPVCs(); err != nil {
					t.Fatal(err)
				}
				saved := &marklogicv1.MarklogicGroup{}
				if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(group), saved); err != nil {
					t.Fatal(err)
				}
				var reports []marklogicv1.UnusedPVCStatus
				if saved.Status.Storage != nil {
					reports = saved.Status.Storage.UnusedPVCs
				}
				if len(reports) != want {
					t.Fatalf("expected %d unused claims, got %+v", want, reports)
				}
				for _, report := range reports {
					if report.Reason != "ScaleUpPending" || report.UID == "" || report.FirstObservedTime.IsZero() {
						t.Fatalf("failed scale-up must report identified prepared claims for reuse: %+v", report)
					}
				}
			}
			outcome, err := operatorContext.ReconcileStatefulset()
			if test.failUpdateOnce {
				if !apierrors.IsConflict(err) {
					t.Fatalf("expected replica update conflict, got %v", err)
				}
				assertUnused(2)
				_, err = operatorContext.ReconcileStatefulset()
			}
			if test.partialCreate {
				if err != nil || outcome.RequeueAfter <= 0 {
					t.Fatalf("expected partial PVC creation to preserve unrelated updates and retry, got %+v, %v", outcome, err)
				}
				assertUnused(1)
				_, err = operatorContext.ReconcileStatefulset()
			}
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			if test.wantError && patchCalls != 0 {
				t.Fatal("unverified or terminating workloads must not receive mutable updates")
			}
			if test.invalidVAC && createCalls != 0 {
				t.Fatal("invalid VAC validation must not create scale-up PVCs")
			}
			if test.wantBlocked {
				if outcome.RequeueAfter <= 0 {
					t.Fatalf("blocked scale-up must request a retry, got %+v", outcome)
				}
				warned := false
				for len(recorder.Events) > 0 {
					warned = strings.Contains(<-recorder.Events, "ScaleUpPVCPreparationFailed") || warned
				}
				if !warned {
					t.Fatal("blocked scale-up must retain its preparation diagnostic")
				}
			}
			if test.failUpdateOnce || test.partialCreate {
				assertUnused(0)
			}
			actual := &appsv1.StatefulSet{}
			if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(current), actual); err != nil {
				t.Fatal(err)
			}
			wantReplicas := desiredReplicas
			if test.wantError || test.wantBlocked {
				wantReplicas = liveReplicas
			}
			if *actual.Spec.Replicas != wantReplicas {
				t.Fatalf("expected %d replicas, got %d", wantReplicas, *actual.Spec.Replicas)
			}
			if !reflect.DeepEqual(actual.Spec.VolumeClaimTemplates, current.Spec.VolumeClaimTemplates) || !reflect.DeepEqual(actual.Spec.Ordinals, current.Spec.Ordinals) || !reflect.DeepEqual(actual.Spec.PersistentVolumeClaimRetentionPolicy, current.Spec.PersistentVolumeClaimRetentionPolicy) {
				t.Fatal("scale-up must preserve claim templates, starting ordinals, and retention policy")
			}
			if !test.wantError {
				if actual.Spec.Template.Spec.Containers[0].Image != group.Spec.Image || !reflect.DeepEqual(actual.Spec.Template.Spec.NodeSelector, group.Spec.NodeSelector) {
					t.Fatal("PVC preparation must not block unrelated image and scheduling updates")
				}
			}
			if !test.wantError && !test.wantBlocked {
				for ordinal := test.start + liveReplicas; ordinal < test.start+desiredReplicas; ordinal++ {
					claim := &corev1.PersistentVolumeClaim{}
					if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: fmt.Sprintf("datadir-%s-%d", current.Name, ordinal)}, claim); err != nil {
						t.Fatal(err)
					}
					if retained != nil && claim.Name == retained.Name {
						if !reflect.DeepEqual(claim.Spec, retained.Spec) {
							t.Fatal("reactivated Pending PVC must not be retargeted")
						}
					} else if !reflect.DeepEqual(claim.Spec, newTemplate.Spec) || claim.Annotations["storage-test"] != "preserved" || claim.Annotations[dataPVCGroupUIDAnnotation] != string(group.UID) || len(claim.OwnerReferences) != 0 {
						t.Fatal("new PVC must preserve current settings, metadata, and retention semantics")
					}
				}
			}
			unchanged := &corev1.PersistentVolumeClaim{}
			if err := fakeClient.Get(operatorContext.Ctx, client.ObjectKeyFromObject(active), unchanged); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unchanged.Spec, active.Spec) {
				t.Fatal("existing active data PVC must remain unchanged")
			}
		})
	}
}

func TestDay1SampleProvisioning(t *testing.T) {
	sample, err := os.Open("../../config/samples/volume-attributes-class.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer sample.Close()
	decoder := k8syaml.NewYAMLOrJSONDecoder(sample, 4096)
	clusters := 0
	claims := 0
	for {
		var cluster marklogicv1.MarklogicCluster
		if err := decoder.Decode(&cluster); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		context := &ClusterContext{MarklogicCluster: &cluster}
		clusterParams := generateMarkLogicClusterParams(&cluster)
		wantClass := ""
		if cluster.Spec.Persistence != nil && cluster.Spec.Persistence.VolumeAttributesClassName != nil {
			wantClass = *cluster.Spec.Persistence.VolumeAttributesClassName
		}
		for index := range cluster.Spec.MarkLogicGroups {
			params := generateMarkLogicGroupParams(&cluster, index, clusterParams)
			group := context.GenerateMarkLogicGroupDef(&cluster, index, params)
			assertPVCClass(t, generateStatefulSetsParams(group).PersistentVolumeClaim, wantClass)
			claims += int(*group.Spec.Replicas)
		}
		clusters++
	}
	if clusters != 2 || claims != 4 {
		t.Fatalf("expected two sample clusters and four data claims, got %d and %d", clusters, claims)
	}
}
