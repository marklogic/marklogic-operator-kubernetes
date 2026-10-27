package utils

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOperatorPodNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifications, notify := OperatorPodNotifications(ctx)
	for _, object := range []interface{}{nil, (*corev1.Pod)(nil), &corev1.Secret{}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}}} {
		notify(object)
	}
	select {
	case pod := <-notifications:
		t.Fatalf("unexpected notification for a non-operator object: %+v", pod)
	default:
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-operator-controller-first"}}
	notify(pod)
	pod.Name = "changed-after-event"
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for index := 0; index < 1000; index++ {
			notify(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("marklogic-operator-controller-%d", index)}})
		}
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("repeated Add events must not block while the notification buffer is full")
	}
	if observed := <-notifications; observed.Name != "marklogic-operator-controller-first" {
		t.Fatalf("the first event must remain a stable snapshot: %+v", observed)
	}
	cancel()
	for index := 0; index < 1000; index++ {
		notify(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-operator-controller-late"}})
	}
	select {
	case observed, open := <-notifications:
		t.Fatalf("cancellation must ignore late events without closing the data channel: pod=%+v, open=%t", observed, open)
	default:
	}
}

func TestOperatorPodNotificationsConcurrentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, notify := OperatorPodNotifications(ctx)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for index := 0; index < 1000; index++ {
				notify(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-operator-controller-concurrent"}})
			}
		}()
	}
	close(start)
	cancel()
	finished := make(chan struct{})
	go func() { workers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("callbacks must complete when cancellation races with Pod events")
	}
}
