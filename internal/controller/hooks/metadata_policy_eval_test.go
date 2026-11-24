package hooks

import (
	"context"
	"encoding/json"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/api/admissionregistration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

// admit runs a rendered MutatingAdmissionPolicy and its binding on obj at
// CREATE, as the API server does (k8s.io/apiserver's own CEL compiler and
// JSON Patch, compatibility version of this module's Kubernetes): the
// namespaceSelectors, the objectSelector, the matchConditions
// (which, as in the API server, see no namespaceObject), then each
// mutation. It returns the object as stored and whether the policy
// changed it.
func admit(t *testing.T, policy, binding *unstructured.Unstructured, obj runtime.Object,
	gvr schema.GroupVersionResource, ns *corev1.Namespace) (runtime.Object, bool) {
	t.Helper()
	var spec v1alpha1.MutatingAdmissionPolicySpec
	var bspec v1alpha1.MutatingAdmissionPolicyBindingSpec
	fromUnstructured(t, policy.Object["spec"], &spec)
	fromUnstructured(t, binding.Object["spec"], &bspec)
	if bspec.PolicyName != policy.GetName() {
		t.Fatalf("binding %s binds %q, not %s", binding.GetName(), bspec.PolicyName, policy.GetName())
	}
	if !ruleMatches(spec.MatchConstraints, gvr) {
		return obj, false
	}
	if !selects(t, spec.MatchConstraints.NamespaceSelector, ns.Labels) ||
		(bspec.MatchResources != nil && !selects(t, bspec.MatchResources.NamespaceSelector, ns.Labels)) {
		return obj, false
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !selects(t, spec.MatchConstraints.ObjectSelector, accessor.GetLabels()) {
		return obj, false
	}

	compiler, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true))
	if err != nil {
		t.Fatal(err)
	}
	opts := plugincel.OptionalVariableDeclarations{StrictCost: true, HasAuthorizer: true}
	vars := make([]plugincel.NamedExpressionAccessor, len(spec.Variables))
	for i, v := range spec.Variables {
		vars[i] = &mutating.Variable{Name: v.Name, Expression: v.Expression}
	}
	compiler.CompileAndStoreVariables(vars, opts, environment.StoredExpressions)
	conditions := make([]plugincel.ExpressionAccessor, len(spec.MatchConditions))
	for i := range spec.MatchConditions {
		conditions[i] = (*matchconditions.MatchCondition)(&spec.MatchConditions[i])
	}
	ignore := admissionregistrationv1.Ignore
	if spec.FailurePolicy == nil || *spec.FailurePolicy != v1alpha1.Ignore {
		t.Fatalf("policy %s: failurePolicy %v, want Ignore", policy.GetName(), spec.FailurePolicy)
	}
	matcher := matchconditions.NewMatcher(compiler.CompileCondition(conditions, opts, environment.StoredExpressions),
		&ignore, "policy", "mutate", policy.GetName())
	patchOpts := opts
	patchOpts.HasPatchTypes = true
	patchers := make([]patch.Patcher, 0, len(spec.Mutations))
	for _, m := range spec.Mutations {
		if m.PatchType != v1alpha1.PatchTypeJSONPatch || m.JSONPatch == nil {
			t.Fatalf("policy %s: a mutation is not JSONPatch", policy.GetName())
		}
		eval := compiler.CompileMutatingEvaluator(&patch.JSONPatchCondition{Expression: m.JSONPatch.Expression},
			patchOpts, environment.StoredExpressions)
		if errs := eval.CompilationErrors(); len(errs) > 0 {
			t.Fatalf("policy %s does not compile: %v", policy.GetName(), errs)
		}
		patchers = append(patchers, patch.NewJSONPatcher(eval))
	}

	ctx := compiler.CompositionEnv.CreateContext(context.Background())
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if _, typed := obj.(*corev1.Pod); typed {
		gvk = corev1.SchemeGroupVersion.WithKind("Pod")
	}
	attrs := admission.NewAttributesRecord(obj, nil, gvk, ns.Name, accessor.GetName(), gvr, "",
		admission.Create, &metav1.CreateOptions{}, false, nil)
	versioned := &admission.VersionedAttributes{Attributes: attrs, VersionedKind: gvk, VersionedObject: obj}
	match := matcher.Match(ctx, versioned, nil, nil)
	if match.Error != nil {
		t.Fatalf("policy %s: match conditions: %v", policy.GetName(), match.Error)
	}
	if !match.Matches {
		t.Logf("policy %s: not matched (condition %q)", policy.GetName(), match.FailedConditionName)
		return obj, false
	}
	for _, p := range patchers {
		out, err := p.Patch(ctx, patch.Request{
			MatchedResource: gvr, VersionedAttributes: versioned,
			ObjectInterfaces:  admission.NewObjectInterfacesFromScheme(scheme),
			OptionalVariables: plugincel.OptionalVariableBindings{}, Namespace: ns,
		}, celconfig.RuntimeCELCostBudget)
		if err != nil {
			t.Fatalf("policy %s: mutation: %v", policy.GetName(), err)
		}
		versioned.VersionedObject = out
	}
	return versioned.VersionedObject, true
}

func fromUnstructured(t *testing.T, in, out any) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func selects(t *testing.T, selector *metav1.LabelSelector, set map[string]string) bool {
	t.Helper()
	if selector == nil {
		return true
	}
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	return s.Matches(labels.Set(set))
}

func ruleMatches(constraints *v1alpha1.MatchResources, gvr schema.GroupVersionResource) bool {
	if constraints == nil {
		return false
	}
	in := func(list []string, s string) bool {
		for _, v := range list {
			if v == s || v == "*" {
				return true
			}
		}
		return false
	}
	for _, rule := range constraints.ResourceRules {
		if in(rule.APIGroups, gvr.Group) && in(rule.APIVersions, gvr.Version) && in(rule.Resources, gvr.Resource) &&
			len(rule.Operations) > 0 {
			for _, op := range rule.Operations {
				if op == admissionregistrationv1.Create {
					return true
				}
			}
		}
	}
	return false
}
