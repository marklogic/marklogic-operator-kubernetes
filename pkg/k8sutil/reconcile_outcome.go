// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"errors"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileOutcome combines the results of independently attempted reconcile steps.
//
// Any error wins and is returned joined, because controller-runtime ignores RequeueAfter
// alongside an error and applies its own backoff. Without an error, an immediate requeue
// wins over the earliest positive delay.
type reconcileOutcome struct {
	errs      []error
	immediate bool
	delay     time.Duration
}

func (o *reconcileOutcome) add(res reconcile.Result, err error) {
	if err != nil {
		o.errs = append(o.errs, err)
		return
	}
	if res.RequeueAfter > 0 {
		if o.delay == 0 || res.RequeueAfter < o.delay {
			o.delay = res.RequeueAfter
		}
		return
	}
	if res.Requeue {
		o.immediate = true
	}
}

// addStep records a step that may be Continue, which requests nothing.
func (o *reconcileOutcome) addStep(step result.ReconcileResult) {
	if !step.Completed() {
		return
	}
	res, err := step.Output()
	o.add(res, err)
}

func (o *reconcileOutcome) output() (reconcile.Result, error) {
	if len(o.errs) > 0 {
		return reconcile.Result{}, errors.Join(o.errs...)
	}
	if o.immediate {
		return reconcile.Result{Requeue: true}, nil
	}
	if o.delay > 0 {
		return reconcile.Result{Requeue: true, RequeueAfter: o.delay}, nil
	}
	return reconcile.Result{}, nil
}
