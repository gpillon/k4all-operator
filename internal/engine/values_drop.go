package engine

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

// A Helm release the operator did not write last (installed by hand,
// restored from k4all 1.x, upgraded by hand while its component was
// Unmanaged) holds values of its own. The operator upgrades without
// ReuseValues, with only its own values (the ReleaseManifest's, the hook's,
// then componentOverrides): every value of the release it does not set
// would go, silently (gpillon/k4all#114: Cilium's k8sServiceHost, and with
// it the API server, on a cluster without kube-proxy). Before its first
// upgrade of such a release the operator compares the two, and holds the
// upgrade when a value would be dropped or changed: ValuesDropError.
//
// Who wrote a revision is read from the API server, not from the operator's
// own records: the field manager that created the Secret Helm keeps the
// revision in. The API server sets it from the client's user agent, so a
// revision written by `helm` reads "Helm", one restored with kubectl reads
// kubectl's, and one written by this operator, of this version or an older
// one (same binary, same user agent), reads the operator's: a cluster whose
// component k4all-operator upgraded last needs no step, and a lost
// ReleaseManifest status changes nothing. Helm carries a release's custom
// labels from one revision to the next, also through `helm upgrade`: a label
// would not tell.

// ValuesDropError says the operator did not upgrade a Helm release it did not
// write last: the values it applies would drop or change some of the
// release's own.
type ValuesDropError struct {
	// Release is the Helm release and Revision its last revision, the one
	// compared.
	Release  string
	Revision int
	// WrittenBy is the field manager that wrote Revision ("" when it cannot
	// be told).
	WrittenBy string
	// Paths are the release's values that would be dropped or changed, as
	// dotted paths (a dot inside a key is escaped, as `helm --set` does),
	// sorted.
	Paths []string
}

func (e *ValuesDropError) Error() string {
	return fmt.Sprintf("revision %d of the Helm release %s was written by %s, not by k4all-operator, "+
		"and the values k4all applies would drop or change these of its values: %s",
		e.Revision, e.Release, e.writer(), strings.Join(e.Paths, ", "))
}

func (e *ValuesDropError) writer() string {
	if e.WrittenBy == "" {
		return "a writer that cannot be told"
	}
	return e.WrittenBy
}

// AllowValuesDrop makes a Helm component's apply go on over a release it did
// not write last when allowed(revision), the release's last revision, is
// true, though its values would be dropped (ValuesDropError).
func AllowValuesDrop(allowed func(revision int) bool) ReconcileOption {
	return func(o *reconcileOptions) { o.allowValuesDrop = allowed }
}

// DroppedValues lists the values of a release (running, its user-supplied
// values) that applying applied, without ReuseValues, would drop or change:
// the leaves of running missing from applied or set there to another value.
// Values applied adds are not listed. A list is one value. An empty map is
// none: Helm merges it with the chart's.
func DroppedValues(running, applied map[string]interface{}) []string {
	r, a := normalizeValues(running), normalizeValues(applied)
	var paths []string
	droppedLeaves(r, a, nil, &paths)
	sort.Strings(paths)
	return paths
}

// normalizeValues is values as Helm stores them: decoded from JSON, so a
// number is a float64 and a list a []interface{} whatever built it.
func normalizeValues(values map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	raw, err := json.Marshal(values)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		return values
	}
	return out
}

func droppedLeaves(running map[string]interface{}, applied interface{}, prefix []string, out *[]string) {
	appliedMap, _ := applied.(map[string]interface{})
	for key, value := range running {
		path := append(prefix[:len(prefix):len(prefix)], key)
		got, set := appliedMap[key]
		if sub, isMap := value.(map[string]interface{}); isMap {
			droppedLeaves(sub, got, path, out)
			continue
		}
		if !set || !reflect.DeepEqual(value, got) {
			*out = append(*out, valuesPath(path))
		}
	}
}

// valuesPath joins the keys of a value with dots, a dot in a key escaped.
func valuesPath(keys []string) string {
	escaped := make([]string, len(keys))
	for i, k := range keys {
		escaped[i] = strings.ReplaceAll(k, ".", `\.`)
	}
	return strings.Join(escaped, ".")
}

// checkTakeover returns a ValuesDropError when the last revision of the
// release was not written by this operator and applying values would drop
// or change some of its values, unless allowed says that revision may lose
// them. No release, or one this operator wrote last, is no takeover.
func (h *HelmInstaller) checkTakeover(
	ctx context.Context, cfg *action.Configuration, name, namespace string, values map[string]interface{},
	allowed func(revision int) bool,
) error {
	last, err := cfg.Releases.Last(name)
	if goerrors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("helm release %s: %w", name, err)
	}
	writer, err := h.revisionWriter(ctx, namespace, last)
	if err != nil {
		return fmt.Errorf("tell who wrote revision %d of the Helm release %s: %w", last.Version, name, err)
	}
	if writer != "" && writer == h.fieldManager {
		return nil
	}
	dropped := DroppedValues(last.Config, values)
	if len(dropped) == 0 {
		return nil
	}
	if allowed != nil && allowed(last.Version) {
		h.log.Info("taking over a helm release written by another manager: its values are dropped, as allowed",
			"name", name, "revision", last.Version, "writtenBy", writer, "dropped", dropped)
		return nil
	}
	return &ValuesDropError{Release: name, Revision: last.Version, WrittenBy: writer, Paths: dropped}
}

// helmSecretNamePrefix starts the name of the Secret Helm's storage keeps a
// revision in: <prefix>.<release>.v<revision>.
const helmSecretNamePrefix = "sh.helm.release.v1"

var secretsResource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// secretCreator returns, as revisionWriter, the field manager that created
// the Secret of a revision: the one that owns its type, which only a create
// sets (Helm rewrites the payload and the labels of a revision when its
// status changes, never its type). Only the Secret's metadata is read.
func secretCreator(restConfig *rest.Config) func(context.Context, string, *release.Release) (string, error) {
	return func(ctx context.Context, namespace string, rel *release.Release) (string, error) {
		client, err := metadata.NewForConfig(restConfig)
		if err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s.%s.v%d", helmSecretNamePrefix, rel.Name, rel.Version)
		obj, err := client.Resource(secretsResource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return creator(obj.ManagedFields), nil
	}
}

// creator is the manager owning the type of a Secret, "" when none does
// (managedFields cleared by hand).
func creator(entries []metav1.ManagedFieldsEntry) string {
	for _, e := range entries {
		if e.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(e.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		if _, ok := fields["f:type"]; ok {
			return e.Manager
		}
	}
	return ""
}

// fieldManagerOf is the field manager the API server records for the writes
// of a client built from restConfig that names none, as Helm's storage
// does: the client's user agent up to its first "/" ("manager", the
// operator's binary, in its container). client-go gives a client without a
// user agent its default one.
func fieldManagerOf(restConfig *rest.Config) string {
	userAgent := rest.DefaultKubernetesUserAgent()
	if restConfig != nil && restConfig.UserAgent != "" {
		userAgent = restConfig.UserAgent
	}
	manager, _, _ := strings.Cut(userAgent, "/")
	return manager
}
