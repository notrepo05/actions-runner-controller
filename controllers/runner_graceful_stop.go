package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/actions-runner-controller/actions-runner-controller/github"
	"github.com/go-logr/logr"
	gogithub "github.com/google/go-github/v39/github"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tickRunnerGracefulStop reconciles the runner and the runner pod in a way so that
// we can delete the runner pod without disrupting a workflow job.
//
// This function returns a non-nil pointer to corev1.Pod as the first return value
// if the runner is considered to have gracefully stopped, hence it's pod is safe for deletion.
//
// It's a "tick" operation so a graceful stop can take multiple calls to complete.
// This function is designed to complete a lengthy graceful stop process in a unblocking way.
// When it wants to be retried later, the function returns a non-nil *ctrl.Result as the second return value, may or may not populating the error in the second return value.
// The caller is expected to return the returned ctrl.Result and error to postpone the current reconcilation loop and trigger a scheduled retry.
func tickRunnerGracefulStop(ctx context.Context, unregistrationTimeout time.Duration, retryDelay time.Duration, log logr.Logger, ghClient *github.Client, c client.Client, enterprise, organization, repository, runner string, pod *corev1.Pod) (*corev1.Pod, *ctrl.Result, error) {
	pod, err := annotatePodOnce(ctx, c, log, pod, AnnotationKeyUnregistrationStartTimestamp, time.Now().Format(time.RFC3339))
	if err != nil {
		return nil, &ctrl.Result{}, err
	}

	if res, err := ensureRunnerUnregistration(ctx, unregistrationTimeout, retryDelay, log, ghClient, enterprise, organization, repository, runner, pod); res != nil {
		return nil, res, err
	}

	pod, err = annotatePodOnce(ctx, c, log, pod, AnnotationKeyUnregistrationCompleteTimestamp, time.Now().Format(time.RFC3339))
	if err != nil {
		return nil, &ctrl.Result{}, err
	}

	return pod, nil, nil
}

// annotatePodOnce annotates the pod if it wasn't.
// Returns the provided pod as-is if it was already annotated.
// Returns the updated pod if the pod was missing the annotation and the update to add the annotation succeeded.
func annotatePodOnce(ctx context.Context, c client.Client, log logr.Logger, pod *corev1.Pod, k, v string) (*corev1.Pod, error) {
	if pod == nil {
		return nil, nil
	}

	if _, ok := getAnnotation(pod, k); ok {
		return pod, nil
	}

	updated := pod.DeepCopy()
	setAnnotation(&updated.ObjectMeta, k, v)
	if err := c.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
		log.Error(err, fmt.Sprintf("Failed to patch pod to have %s annotation", k))
		return nil, err
	}

	log.V(2).Info("Annotated pod", "key", k, "value", v)

	return updated, nil
}

// If the first return value is nil, it's safe to delete the runner pod.
func ensureRunnerUnregistration(ctx context.Context, unregistrationTimeout time.Duration, retryDelay time.Duration, log logr.Logger, ghClient *github.Client, enterprise, organization, repository, runner string, pod *corev1.Pod) (*ctrl.Result, error) {
	var runnerID *int64

	if id, ok := getAnnotation(pod, AnnotationKeyRunnerID); ok {
		v, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return &ctrl.Result{}, err
		}

		runnerID = &v
	}

	ok, err := unregisterRunner(ctx, ghClient, enterprise, organization, repository, runner, runnerID)
	if err != nil {
		if errors.Is(err, &gogithub.RateLimitError{}) {
			// We log the underlying error when we failed calling GitHub API to list or unregisters,
			// or the runner is still busy.
			log.Error(
				err,
				fmt.Sprintf(
					"Failed to unregister runner due to GitHub API rate limits. Delaying retry for %s to avoid excessive GitHub API calls",
					retryDelayOnGitHubAPIRateLimitError,
				),
			)

			return &ctrl.Result{RequeueAfter: retryDelayOnGitHubAPIRateLimitError}, err
		}

		log.Error(err, "Failed to unregister runner before deleting the pod.")

		errRes := &gogithub.ErrorResponse{}
		if errors.As(err, &errRes) {
			code := runnerContainerExitCode(pod)

			runner, _ := getRunner(ctx, ghClient, enterprise, organization, repository, runner)

			var runnerID int64

			if runner != nil && runner.ID != nil {
				runnerID = *runner.ID
			}

			if errRes.Response.StatusCode == 422 && code != nil {
				log.V(2).Info("Runner container has already stopped but the unregistration attempt failed. "+
					"This can happen when the runner container crashed due to an unhandled error, OOM, etc. "+
					"ARC terminates the pod anyway. You'd probably need to manually delete the runner later by calling the GitHub API",
					"runnerExitCode", *code,
					"runnerID", runnerID,
				)

				return nil, nil
			}
		}

		return &ctrl.Result{}, err
	} else if ok {
		log.Info("Runner has just been unregistered.")
	} else if pod == nil {
		// `r.unregisterRunner()` will returns `false, nil` if the runner is not found on GitHub.
		// However, that doesn't always mean the pod can be safely removed.
		//
		// If the pod does not exist for the runner,
		// it may be due to that the runner pod has never been created.
		// In that case we can safely assume that the runner will never be registered.

		log.Info("Runner was not found on GitHub and the runner pod was not found on Kuberntes.")
	} else if pod.Annotations[AnnotationKeyUnregistrationCompleteTimestamp] != "" {
		// If it's already unregistered in the previous reconcilation loop,
		// you can safely assume that it won't get registered again so it's safe to delete the runner pod.
		log.Info("Runner pod is marked as already unregistered.")
	} else if runnerPodOrContainerIsStopped(pod) {
		// If it's an ephemeral runner with the actions/runner container exited with 0,
		// we can safely assume that it has unregistered itself from GitHub Actions
		// so it's natural that RemoveRunner fails due to 404.

		// If pod has ended up succeeded we need to restart it
		// Happens e.g. when dind is in runner and run completes
		log.Info("Runner pod has been stopped with a successful status.")
	} else if ts := pod.Annotations[AnnotationKeyUnregistrationStartTimestamp]; ts != "" {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return &ctrl.Result{RequeueAfter: retryDelay}, err
		}

		if r := time.Until(t.Add(unregistrationTimeout)); r > 0 {
			log.Info("Runner unregistration is in-progress.", "timeout", unregistrationTimeout, "remaining", r)
			return &ctrl.Result{RequeueAfter: retryDelay}, err
		}

		log.Info("Runner unregistration has been timed out. The runner pod will be deleted soon.", "timeout", unregistrationTimeout)
	} else {
		// A runner and a runner pod that is created by this version of ARC should match
		// any of the above branches.
		//
		// But we leave this match all branch for potential backward-compatibility.
		// The caller is expected to take appropriate actions, like annotating the pod as started the unregistration process,
		// and retry later.
		log.V(1).Info("Runner unregistration is being retried later.")

		return &ctrl.Result{RequeueAfter: retryDelay}, nil
	}

	return nil, nil
}

func ensureRunnerPodRegistered(ctx context.Context, log logr.Logger, ghClient *github.Client, c client.Client, enterprise, organization, repository, runner string, pod *corev1.Pod) (*corev1.Pod, *ctrl.Result, error) {
	_, hasRunnerID := getAnnotation(pod, AnnotationKeyRunnerID)
	if runnerPodOrContainerIsStopped(pod) || hasRunnerID {
		return pod, nil, nil
	}

	r, err := getRunner(ctx, ghClient, enterprise, organization, repository, runner)
	if err != nil {
		return nil, &ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if r == nil || r.ID == nil {
		return nil, &ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	id := *r.ID

	updated, err := annotatePodOnce(ctx, c, log, pod, AnnotationKeyRunnerID, fmt.Sprintf("%d", id))
	if err != nil {
		return nil, &ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	return updated, nil, nil
}

func getAnnotation(obj client.Object, key string) (string, bool) {
	if obj.GetAnnotations() == nil {
		return "", false
	}

	v, ok := obj.GetAnnotations()[key]

	return v, ok
}

func setAnnotation(meta *metav1.ObjectMeta, key, value string) {
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}

	meta.Annotations[key] = value
}

func podConditionTransitionTime(pod *corev1.Pod, tpe corev1.PodConditionType, v corev1.ConditionStatus) *metav1.Time {
	for _, c := range pod.Status.Conditions {
		if c.Type == tpe && c.Status == v {
			return &c.LastTransitionTime
		}
	}

	return nil
}

func podConditionTransitionTimeAfter(pod *corev1.Pod, tpe corev1.PodConditionType, d time.Duration) bool {
	c := podConditionTransitionTime(pod, tpe, corev1.ConditionTrue)
	if c == nil {
		return false
	}

	return c.Add(d).Before(time.Now())
}

func podRunnerID(pod *corev1.Pod) string {
	id, _ := getAnnotation(pod, AnnotationKeyRunnerID)
	return id
}

// unregisterRunner unregisters the runner from GitHub Actions by name.
//
// This function returns:
//
// Case 1. (true, nil) when it has successfully unregistered the runner.
// Case 2. (false, nil) when (2-1.) the runner has been already unregistered OR (2-2.) the runner will never be created OR (2-3.) the runner is not created yet and it is about to be registered(hence we couldn't see it's existence from GitHub Actions API yet)
// Case 3. (false, err) when it postponed unregistration due to the runner being busy, or it tried to unregister the runner but failed due to
//   an error returned by GitHub API.
//
// When the returned values is "Case 2. (false, nil)", the caller must handle the three possible sub-cases appropriately.
// In other words, all those three sub-cases cannot be distinguished by this function alone.
//
// - Case "2-1." can happen when e.g. ARC has successfully unregistered in a previous reconcilation loop or it was an ephemeral runner that finished it's job run(an ephemeral runner is designed to stop after a job run).
//   You'd need to maintain the runner state(i.e. if it's already unregistered or not) somewhere,
//   so that you can either not call this function at all if the runner state says it's already unregistered, or determine that it's case "2-1." when you got (false, nil).
//
// - Case "2-2." can happen when e.g. the runner registration token was somehow broken so that `config.sh` within the runner container was never meant to succeed.
//   Waiting and retrying forever on this case is not a solution, because `config.sh` won't succeed with a wrong token hence the runner gets stuck in this state forever.
//   There isn't a perfect solution to this, but a practical workaround would be implement a "grace period" in the caller side.
//
// - Case "2-3." can happen when e.g. ARC recreated an ephemral runner pod in a previous reconcilation loop and then it was requested to delete the runner before the runner comes up.
//   If handled inappropriately, this can cause a race condition betweeen a deletion of the runner pod and GitHub scheduling a workflow job onto the runner.
//
// Once successfully detected case "2-1." or "2-2.", you can safely delete the runner pod because you know that the runner won't come back
// as long as you recreate the runner pod.
//
// If it was "2-3.", you need a workaround to avoid the race condition.
//
// You shall introduce a "grace period" mechanism, similar or equal to that is required for "Case 2-2.", so that you ever
// start the runner pod deletion only after it's more and more likely that the runner pod is not coming up.
//
// Beware though, you need extra care to set an appropriate grace period depending on your environment.
// There isn't a single right grace period that works for everyone.
// The longer the grace period is, the earlier a cluster resource shortage can occur due to throttoled runner pod deletions,
// while the shorter the grace period is, the more likely you may encounter the race issue.
func unregisterRunner(ctx context.Context, client *github.Client, enterprise, org, repo, name string, id *int64) (bool, error) {
	if id == nil {
		runner, err := getRunner(ctx, client, enterprise, org, repo, name)
		if err != nil {
			return false, err
		}

		if runner == nil || runner.ID == nil {
			return false, nil
		}

		id = runner.ID
	}

	// For the record, historically ARC did not try to call RemoveRunner on a busy runner, but it's no longer true.
	// The reason ARC did so was to let a runner running a job to not stop prematurely.
	//
	// However, we learned that RemoveRunner already has an ability to prevent stopping a busy runner,
	// so ARC doesn't need to do anything special for a graceful runner stop.
	// It can just call RemoveRunner, and if it returned 200 you're guaranteed that the runner will not automatically come back and
	// the runner pod is safe for deletion.
	//
	// Trying to remove a busy runner can result in errors like the following:
	//    failed to remove runner: DELETE https://api.github.com/repos/actions-runner-controller/mumoshu-actions-test/actions/runners/47: 422 Bad request - Runner \"example-runnerset-0\" is still running a job\" []
	//
	// # NOTES
	//
	// - It can be "status=offline" at the same time but that's another story.
	// - After https://github.com/actions-runner-controller/actions-runner-controller/pull/1127, ListRunners responses that are used to
	//   determine if the runner is busy can be more outdated than before, as those responeses are now cached for 60 seconds.
	// - Note that 60 seconds is controlled by the Cache-Control response header provided by GitHub so we don't have a strict control on it but we assume it won't
	//   change from 60 seconds.
	//
	// TODO: Probably we can just remove the runner by ID without seeing if the runner is busy, by treating it as busy when a remove-runner call failed with 422?
	if err := client.RemoveRunner(ctx, enterprise, org, repo, *id); err != nil {
		return false, err
	}

	return true, nil
}

func getRunner(ctx context.Context, client *github.Client, enterprise, org, repo, name string) (*gogithub.Runner, error) {
	runners, err := client.ListRunners(ctx, enterprise, org, repo)
	if err != nil {
		return nil, err
	}

	for _, runner := range runners {
		if runner.GetName() == name {
			return runner, nil
		}
	}

	return nil, nil
}
