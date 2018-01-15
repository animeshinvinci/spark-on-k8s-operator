/*
Copyright 2017 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/spark-on-k8s-operator/pkg/apis/sparkoperator.k8s.io/v1alpha1"
	"k8s.io/spark-on-k8s-operator/pkg/crd"
	"k8s.io/spark-on-k8s-operator/pkg/util"
	crdclientset "k8s.io/spark-on-k8s-operator/pkg/client/clientset/versioned"
	crdinformers "k8s.io/spark-on-k8s-operator/pkg/client/informers/externalversions"

	apiv1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"strconv"
)

const (
	sparkRoleLabel       = "spark-role"
	sparkDriverRole      = "driver"
	sparkExecutorRole    = "executor"
	sparkExecutorIDLabel = "spark-exec-id"
	maximumUpdateRetries = 3
)

// SparkApplicationController manages instances of SparkApplication.
type SparkApplicationController struct {
	crdClient                  crdclientset.Interface
	kubeClient                 clientset.Interface
	extensionsClient           apiextensionsclient.Interface
	recorder                   record.EventRecorder
	runner                     *sparkSubmitRunner
	sparkPodMonitor            *sparkPodMonitor
	appStateReportingChan      <-chan appStateUpdate
	driverStateReportingChan   <-chan driverStateUpdate
	executorStateReportingChan <-chan executorStateUpdate
	mutex                      sync.Mutex                            // Guard SparkApplication updates to the API server and runningApps.
	runningApps                map[string]*v1alpha1.SparkApplication // Guarded by mutex.
}

// New creates a new SparkApplicationController.
func New(
	crdClient crdclientset.Interface,
	kubeClient clientset.Interface,
	extensionsClient apiextensionsclient.Interface,
	submissionRunnerWorkers int) *SparkApplicationController {
	v1alpha1.AddToScheme(scheme.Scheme)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.V(2).Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: kubeClient.CoreV1().Events(apiv1.NamespaceAll),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: "spark-operator"})

	return newSparkApplicationController(crdClient, kubeClient, extensionsClient, recorder, submissionRunnerWorkers)
}

func newSparkApplicationController(
	crdClient crdclientset.Interface,
	kubeClient clientset.Interface,
	extensionsClient apiextensionsclient.Interface,
	eventRecorder record.EventRecorder,
	submissionRunnerWorkers int) *SparkApplicationController {
	appStateReportingChan := make(chan appStateUpdate, submissionRunnerWorkers)
	driverStateReportingChan := make(chan driverStateUpdate)
	executorStateReportingChan := make(chan executorStateUpdate)

	runner := newSparkSubmitRunner(submissionRunnerWorkers, appStateReportingChan)
	sparkPodMonitor := newSparkPodMonitor(kubeClient, driverStateReportingChan, executorStateReportingChan)

	return &SparkApplicationController{
		crdClient:                  crdClient,
		kubeClient:                 kubeClient,
		extensionsClient:           extensionsClient,
		recorder:                   eventRecorder,
		runner:                     runner,
		sparkPodMonitor:            sparkPodMonitor,
		appStateReportingChan:      appStateReportingChan,
		driverStateReportingChan:   driverStateReportingChan,
		executorStateReportingChan: executorStateReportingChan,
		runningApps:                make(map[string]*v1alpha1.SparkApplication),
	}
}

// Start starts the SparkApplicationController by registering a watcher for SparkApplication objects.
func (s *SparkApplicationController) Start(stopCh <-chan struct{}) error {
	glog.Info("Starting the SparkApplication controller")

	glog.Infof("Creating the CustomResourceDefinition %s", crd.FullName)
	err := crd.CreateCRD(s.extensionsClient)
	if err != nil {
		return fmt.Errorf("failed to create the CustomResourceDefinition %s: %v", crd.FullName, err)
	}

	glog.Info("Starting the SparkApplication informer")
	err = s.startSparkApplicationInformer(stopCh)
	if err != nil {
		return fmt.Errorf("failed to register watch for SparkApplication resource: %v", err)
	}

	go s.runner.run(stopCh)
	go s.sparkPodMonitor.run(stopCh)

	go s.processAppStateUpdates()
	go s.processDriverStateUpdates()
	go s.processExecutorStateUpdates()

	return nil
}

func (s *SparkApplicationController) startSparkApplicationInformer(stopCh <-chan struct{}) error {
	informerFactory := crdinformers.NewSharedInformerFactory(
		s.crdClient,
		// resyncPeriod. Every resyncPeriod, all resources in the cache will re-trigger events.
		// Set to 0 to disable the resync.
		0*time.Second)
	informer := informerFactory.Sparkoperator().V1alpha1().SparkApplications().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.onAdd,
		DeleteFunc: s.onDelete,
	})
	go informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		return fmt.Errorf("timed out waiting for cache to sync")
	}

	return nil
}

// Callback function called when a new SparkApplication object gets created.
func (s *SparkApplicationController) onAdd(obj interface{}) {
	app := obj.(*v1alpha1.SparkApplication)
	s.recorder.Eventf(
		app,
		apiv1.EventTypeNormal,
		"SparkApplicationSubmission",
		"Submitting SparkApplication: %s",
		app.Name)
	s.submitApp(app, false)
}

func (s *SparkApplicationController) onDelete(obj interface{}) {
	app := obj.(*v1alpha1.SparkApplication)

	s.recorder.Eventf(
		app,
		apiv1.EventTypeNormal,
		"SparkApplicationDeletion",
		"Deleting SparkApplication: %s",
		app.Name)

	s.mutex.Lock()
	delete(s.runningApps, app.Status.AppID)
	s.mutex.Unlock()
}

func (s *SparkApplicationController) submitApp(app *v1alpha1.SparkApplication, resubmission bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	updatedApp := s.updateSparkApplicationWithRetries(app, app.DeepCopy(), func(toUpdate *v1alpha1.SparkApplication) {
		if resubmission {
			// Clear the Status field if it's a resubmission.
			toUpdate.Status = v1alpha1.SparkApplicationStatus{}
		}
		toUpdate.Status.AppID = buildAppID(toUpdate)
		toUpdate.Status.AppState.State = v1alpha1.NewState
		createSparkUIService(toUpdate, s.kubeClient)
	})

	if updatedApp == nil {
		return
	}

	s.runningApps[updatedApp.Status.AppID] = updatedApp

	submissionCmdArgs, err := buildSubmissionCommandArgs(updatedApp)
	if err != nil {
		glog.Errorf(
			"failed to build the submission command for SparkApplication %s: %v",
			updatedApp.Name,
			err)
	}
	if !updatedApp.Spec.SubmissionByUser {
		s.runner.submit(newSubmission(submissionCmdArgs, updatedApp))
	}
}

func (s *SparkApplicationController) processDriverStateUpdates() {
	for update := range s.driverStateReportingChan {
		updatedApp := s.processSingleDriverStateUpdate(update)
		if updatedApp != nil && isAppTerminated(updatedApp.Status.AppState.State) {
			s.recorder.Eventf(
				updatedApp,
				apiv1.EventTypeNormal,
				"SparkApplicationTermination",
				"SparkApplication %s terminated with state: %v",
				updatedApp.Name,
				updatedApp.Status.AppState)
			s.handleRestart(updatedApp)
		}
	}
}

func (s *SparkApplicationController) processSingleDriverStateUpdate(update driverStateUpdate) *v1alpha1.SparkApplication {
	glog.V(2).Infof(
		"Received driver state update for %s with phase %s",
		update.appID,
		update.podPhase)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if app, ok := s.runningApps[update.appID]; ok {
		updated := s.updateSparkApplicationWithRetries(app, app.DeepCopy(), func(toUpdate *v1alpha1.SparkApplication) {
			toUpdate.Status.DriverInfo.PodName = update.podName
			if update.nodeName != "" {
				nodeIP := s.getNodeExternalIP(update.nodeName)
				if nodeIP != "" {
					toUpdate.Status.DriverInfo.WebUIAddress = fmt.Sprintf(
						"%s:%d", nodeIP, toUpdate.Status.DriverInfo.WebUIPort)
				}
			}

			appState := driverPodPhaseToApplicationState(update.podPhase)
			// Update the application based on the driver pod phase if the driver has terminated.
			if isAppTerminated(appState) {
				toUpdate.Status.AppState.State = appState
			}
		})

		if updated != nil {
			s.runningApps[updated.Status.AppID] = updated
		}
		return updated
	}

	return nil
}

func (s *SparkApplicationController) processAppStateUpdates() {
	for update := range s.appStateReportingChan {
		s.processSingleAppStateUpdate(update)
	}
}

func (s *SparkApplicationController) processSingleAppStateUpdate(update appStateUpdate) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if app, ok := s.runningApps[update.appID]; ok {
		updated := s.updateSparkApplicationWithRetries(app, app.DeepCopy(), func(toUpdate *v1alpha1.SparkApplication) {
			// The application termination state is set based on the driver pod termination state. So if the app state
			// is already a termination state, skip updating the state here. Otherwise, if the submission runner fails
			// to submit the application, the application state is set to FailedSubmissionState.
			if !isAppTerminated(toUpdate.Status.AppState.State) || update.state == v1alpha1.FailedSubmissionState {
				toUpdate.Status.AppState.State = update.state
				toUpdate.Status.AppState.ErrorMessage = update.errorMessage
			}
			if !update.submissionTime.IsZero() {
				toUpdate.Status.SubmissionTime = update.submissionTime
			}
			if !update.completionTime.IsZero() {
				toUpdate.Status.CompletionTime = update.completionTime
			}
		})

		if updated != nil {
			s.runningApps[updated.Status.AppID] = updated
			if updated.Status.AppState.State == v1alpha1.FailedSubmissionState {
				s.recorder.Eventf(
					updated,
					apiv1.EventTypeNormal,
					"SparkApplicationSubmissionFailure",
					"SparkApplication %s failed submission",
					updated.Name)
			}
		}
	}
}

func (s *SparkApplicationController) processExecutorStateUpdates() {
	for update := range s.executorStateReportingChan {
		s.processSingleExecutorStateUpdate(update)
	}
}

func (s *SparkApplicationController) processSingleExecutorStateUpdate(update executorStateUpdate) {
	glog.V(2).Infof(
		"Received state update of executor %s for %s with state %s",
		update.executorID,
		update.appID,
		update.state)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if app, ok := s.runningApps[update.appID]; ok {
		updated := s.updateSparkApplicationWithRetries(app, app.DeepCopy(), func(toUpdate *v1alpha1.SparkApplication) {
			if toUpdate.Status.ExecutorState == nil {
				toUpdate.Status.ExecutorState = make(map[string]v1alpha1.ExecutorState)
			}
			if update.state != v1alpha1.ExecutorPendingState {
				toUpdate.Status.ExecutorState[update.podName] = update.state
			}
		})

		if updated != nil {
			s.runningApps[updated.Status.AppID] = updated
		}
	}
}

func (s *SparkApplicationController) updateSparkApplicationWithRetries(
	original *v1alpha1.SparkApplication,
	toUpdate *v1alpha1.SparkApplication,
	updateFunc func(*v1alpha1.SparkApplication)) *v1alpha1.SparkApplication {
	for i := 0; i < maximumUpdateRetries; i++ {
		updateFunc(toUpdate)
		if reflect.DeepEqual(original.Status, toUpdate.Status) {
			return nil
		}

		client := s.crdClient.SparkoperatorV1alpha1().SparkApplications(toUpdate.Namespace)
		updated, err := client.Update(toUpdate)
		if err == nil {
			return updated
		}

		// Failed update to the API server.
		// Get the latest version from the API server first and re-apply the update.
		name := toUpdate.Name
		toUpdate, err = client.Get(name, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("failed to get SparkApplication %s: %v", name, err)
			return nil
		}
	}

	return nil
}

func (s *SparkApplicationController) getNodeExternalIP(nodeName string) string {
	node, err := s.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get node %s", nodeName)
		return ""
	}
	for _, address := range node.Status.Addresses {
		if address.Type == apiv1.NodeExternalIP {
			return address.Address
		}
	}
	return ""
}

func (s *SparkApplicationController) handleRestart(app *v1alpha1.SparkApplication) {
	if app.Spec.RestartPolicy == v1alpha1.Never || app.Spec.RestartPolicy == v1alpha1.Undefined {
		return
	}

	if (app.Status.AppState.State == v1alpha1.FailedState && app.Spec.RestartPolicy == v1alpha1.OnFailure) ||
		app.Spec.RestartPolicy == v1alpha1.Always {
		glog.Infof("SparkApplication %s failed or terminated, restarting it with RestartPolicy %s",
			app.Name, app.Spec.RestartPolicy)
		s.recorder.Eventf(
			app,
			apiv1.EventTypeNormal,
			"SparkApplicationResubmission",
			"Re-submitting SparkApplication: %s",
			app.Name)
		s.submitApp(app, true)
	}
}

// buildAppID builds an application ID in the form of <application name>-<32-bit hash>.
func buildAppID(app *v1alpha1.SparkApplication) string {
	hasher := util.NewHash32()
	hasher.Write([]byte(app.Name))
	hasher.Write([]byte(app.Namespace))
	hasher.Write([]byte(app.UID))
	hasher.Write([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return fmt.Sprintf("%s-%d", app.Name, hasher.Sum32())
}

func isAppTerminated(appState v1alpha1.ApplicationStateType) bool {
	return appState == v1alpha1.CompletedState || appState == v1alpha1.FailedState
}

func driverPodPhaseToApplicationState(podPhase apiv1.PodPhase) v1alpha1.ApplicationStateType {
	switch podPhase {
	case apiv1.PodPending:
		return v1alpha1.SubmittedState
	case apiv1.PodRunning:
		return v1alpha1.RunningState
	case apiv1.PodSucceeded:
		return v1alpha1.CompletedState
	case apiv1.PodFailed:
		return v1alpha1.FailedState
	default:
		return ""
	}
}
