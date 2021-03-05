/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package instance_controller groups the functionalities related to the Instance controller.
package instance_controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/errors"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crownlabsv1alpha2 "github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	instance_creation "github.com/netgroup-polito/CrownLabs/operators/pkg/instance-creation"
)

// InstanceReconciler reconciles a Instance object.
type InstanceReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	EventsRecorder     record.EventRecorder
	NamespaceWhitelist metav1.LabelSelector
	WebsiteBaseURL     string
	NextcloudBaseURL   string
	WebdavSecretName   string
	Oauth2ProxyImage   string
	OidcClientSecret   string
	OidcProviderURL    string
}

// Reconcile reconciles the state of an Instance resource.
func (r *InstanceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	VMstart := time.Now()
	ctx := context.Background()

	// get instance
	var instance crownlabsv1alpha2.Instance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		// reconcile was triggered by a delete request
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ns := v1.Namespace{}
	namespaceName := types.NamespacedName{
		Name:      instance.Namespace,
		Namespace: "",
	}

	// It performs reconciliation only if the Instance belongs to whitelisted namespaces
	// by checking the existence of keys in the instance namespace
	if err := r.Get(ctx, namespaceName, &ns); err == nil {
		if !instance_creation.CheckLabels(&ns, r.NamespaceWhitelist.MatchLabels) {
			klog.Info("Namespace " + req.Namespace + " does not meet the selector labels")
			return ctrl.Result{}, nil
		}
	} else {
		klog.Error("Unable to get Instance namespace")
		klog.Error(err)
	}
	klog.Info("Namespace " + req.Namespace + " met the selector labels")
	// The metadata.generation value is incremented for all changes, except for changes to .metadata or .status
	// if metadata.generation is not incremented there's no need to reconcile
	if instance.Status.ObservedGeneration == instance.ObjectMeta.Generation {
		return ctrl.Result{}, nil
	}

	// check if the Template exists
	templateName := types.NamespacedName{
		Namespace: instance.Spec.Template.Namespace,
		Name:      instance.Spec.Template.Name,
	}
	var template crownlabsv1alpha2.Template
	if err := r.Get(ctx, templateName, &template); err != nil {
		// no Template related exists
		klog.Info("Template " + templateName.Name + " doesn't exist.")
		r.EventsRecorder.Event(&instance, "Warning", "TemplateNotFound", "Template "+templateName.Name+" not found in namespace "+template.Namespace)
		return ctrl.Result{}, err
	}

	r.EventsRecorder.Event(&instance, "Normal", "TemplateFound", "Template "+templateName.Name+" found in namespace "+template.Namespace)
	instance.Labels = map[string]string{
		"course-name":        strings.ReplaceAll(strings.ToLower(template.Spec.WorkspaceRef.Name), " ", "-"),
		"template-name":      template.Name,
		"template-namespace": template.Namespace,
	}
	instance.Status.ObservedGeneration = instance.ObjectMeta.Generation
	if err := r.Update(ctx, &instance); err != nil {
		klog.Error("Unable to update Instance labels")
		klog.Error(err)
	}

	if _, err := r.generateEnvironments(&template, &instance, VMstart); err != nil {
		return ctrl.Result{}, err
	}

	// create secret referenced by VirtualMachineInstance (Cloudinit)
	// To be extracted in a configuration flag
	VMElaborationTimestamp := time.Now()
	VMElaborationDuration := VMElaborationTimestamp.Sub(VMstart)
	elaborationTimes.Observe(VMElaborationDuration.Seconds())

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) generateEnvironments(template *crownlabsv1alpha2.Template, instance *crownlabsv1alpha2.Instance, vmstart time.Time) (ctrl.Result, error) {
	name := fmt.Sprintf("%v-%.4s", strings.ReplaceAll(instance.Name, ".", "-"), uuid.New().String())
	namespace := instance.Namespace
	for i := range template.Spec.EnvironmentList {
		// prepare variables common to all resources
		switch template.Spec.EnvironmentList[i].EnvironmentType {
		case crownlabsv1alpha2.ClassVM:
			if err := r.CreateVMEnvironment(instance, &template.Spec.EnvironmentList[i], namespace, name, vmstart); err != nil {
				return ctrl.Result{}, err
			}
		case crownlabsv1alpha2.ClassContainer:
			return ctrl.Result{}, errors.NewBadRequest("Container Environments are not implemented")
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers a new controller for Instance resources.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crownlabsv1alpha2.Instance{}).
		Complete(r)
}

func (r *InstanceReconciler) setInstanceStatus(
	ctx context.Context,
	msg string, eventType string, eventReason string,
	instance *crownlabsv1alpha2.Instance, ip, url string) {
	klog.Info(msg)
	r.EventsRecorder.Event(instance, eventType, eventReason, msg)

	instance.Status.Phase = eventReason
	instance.Status.IP = ip
	instance.Status.URL = url
	instance.Status.ObservedGeneration = instance.ObjectMeta.Generation
	if err := r.Status().Update(ctx, instance); err != nil {
		klog.Error("Unable to update Instance status")
		klog.Error(err)
	}
}