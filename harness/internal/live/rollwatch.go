package live

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
)

// RollWatcher observes the ztunnel DaemonSet pods in istio-system and derives a
// per-node RollWindow: an OLD pod entering Terminating fixes drainingAt (its
// deletionTimestamp) and graceExpiresAt (drainingAt + grace); the NEW pod on
// that node reaching Ready fixes readyAt (a progress marker only).
type RollWatcher struct {
	grace time.Duration

	mu      sync.Mutex
	drain   map[string]time.Time // node -> earliest observed drainingAt
	ready   map[string]time.Time // node -> latest new-pod readyAt
	seenUID map[string]bool      // pod UID -> already recorded draining
}

// NewRollWatcher builds a watcher with the given ztunnel grace period (120s).
func NewRollWatcher(grace time.Duration) *RollWatcher {
	return &RollWatcher{
		grace:   grace,
		drain:   map[string]time.Time{},
		ready:   map[string]time.Time{},
		seenUID: map[string]bool{},
	}
}

// Start runs a shared informer over istio-system pods labelled app=ztunnel until
// ctx is cancelled. It records draining/ready transitions into the watcher.
func (w *RollWatcher) Start(ctx context.Context, cs kubernetes.Interface) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		cs, 0,
		informers.WithNamespace("istio-system"),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = "app=ztunnel"
		}),
	)
	podInformer := factory.Core().V1().Pods().Informer()
	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.onPod(obj) },
		UpdateFunc: func(_, obj any) { w.onPod(obj) },
		DeleteFunc: func(obj any) { w.onPod(obj) },
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	<-ctx.Done()
	return nil
}

func (w *RollWatcher) onPod(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tomb, tok := obj.(cache.DeletedFinalStateUnknown); tok {
			pod, ok = tomb.Obj.(*corev1.Pod)
		}
		if !ok {
			return
		}
	}
	node := pod.Spec.NodeName
	if node == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// Draining: a pod with a deletionTimestamp is terminating. Record the
	// earliest drain seen per node and only once per pod UID.
	if ts := pod.DeletionTimestamp; ts != nil && !w.seenUID[string(pod.UID)] {
		w.seenUID[string(pod.UID)] = true
		d := ts.Time.UTC()
		if cur, ok := w.drain[node]; !ok || d.Before(cur) {
			w.drain[node] = d
		}
	}

	// Ready: a running, Ready pod without a deletionTimestamp is the NEW pod.
	if pod.DeletionTimestamp == nil && podReady(pod) {
		r := time.Now().UTC()
		if c := readyConditionTime(pod); !c.IsZero() {
			r = c
		}
		if cur, ok := w.ready[node]; !ok || r.After(cur) {
			w.ready[node] = r
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func readyConditionTime(pod *corev1.Pod) time.Time {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time.UTC()
		}
	}
	return time.Time{}
}

// Windows snapshots the roll windows observed so far, one per node that started
// draining. graceExpiresAt = drainingAt + grace; readyAt is attached when the
// new pod on that node became Ready (nil => half-open at analysis time).
func (w *RollWatcher) Windows() []model.RollWindow {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]model.RollWindow, 0, len(w.drain))
	for node, d := range w.drain {
		win := model.RollWindow{
			Node:           node,
			DrainingAt:     d,
			GraceExpiresAt: d.Add(w.grace),
		}
		if r, ok := w.ready[node]; ok {
			rc := r
			win.ReadyAt = &rc
		}
		out = append(out, win)
	}
	return out
}
