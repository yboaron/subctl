/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package restconfig

import (
	"fmt"
	"strings"

	"github.com/coreos/go-semver/semver"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"github.com/submariner-io/admiral/pkg/reporter"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/subctl/internal/constants"
	"github.com/submariner-io/subctl/internal/gvr"
	"github.com/submariner-io/subctl/pkg/cluster"
	"github.com/submariner-io/subctl/pkg/version"
	"github.com/submariner-io/submariner-operator/api/v1alpha1"
	subv1 "github.com/submariner-io/submariner/pkg/apis/submariner.io/v1"
	k8serrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

const InCluster = "in-cluster"

type RestConfig struct {
	Config      *rest.Config
	ClusterName string
}

type loadingRulesAndOverrides struct {
	loadingRules *clientcmd.ClientConfigLoadingRules
	overrides    *clientcmd.ConfigOverrides
}

type Producer struct {
	contexts                  []string
	contextPrefixes           []string
	defaultClientConfig       *loadingRulesAndOverrides
	prefixedClientConfigs     map[string]*loadingRulesAndOverrides
	prefixedKubeConfigs       map[string]*string
	inClusterFlag             bool
	inCluster                 bool
	namespaceFlag             bool
	contextsFlag              bool
	defaultNamespace          *string
	prefixedDefaultNamespaces map[string]*string
}

// GetInClusterConfig is exposed for unit tests to override.
var GetInClusterConfig = rest.InClusterConfig

// NewProducer initialises a blank producer which needs to be set up with flags (see SetupFlags).
func NewProducer() *Producer {
	return &Producer{prefixedDefaultNamespaces: make(map[string]*string)}
}

// WithNamespace configures the producer to set up a namespace flag.
// The chosen namespace will be passed to the PerContextFn used to process the context.
func (rcp *Producer) WithNamespace() *Producer {
	rcp.namespaceFlag = true

	return rcp
}

// WithDefaultNamespace configures the producer to set up a namespace flag,
// with the given default value.
// The chosen namespace will be passed to the PerContextFn used to process the context.
func (rcp *Producer) WithDefaultNamespace(defaultNamespace string) *Producer {
	rcp.namespaceFlag = true
	rcp.defaultNamespace = &defaultNamespace

	return rcp
}

// WithPrefixedNamespace configures the producer to set up a prefixed namespace flag,
// with the given default value.
// The chosen namespace will be passed to the PerContextFn used to process the context.
func (rcp *Producer) WithPrefixedNamespace(prefix, prefixedNamespace string) *Producer {
	rcp.prefixedDefaultNamespaces[prefix] = &prefixedNamespace

	return rcp
}

// WithPrefixedContext configures the producer to set up flags using the given prefix.
func (rcp *Producer) WithPrefixedContext(prefix string) *Producer {
	rcp.contextPrefixes = append(rcp.contextPrefixes, prefix)

	return rcp
}

// WithContextsFlag configures the producer to allow multiple contexts to be selected with the --contexts flag.
// This is only usable with RunOnAllContexts and will act as a filter on the selected contexts.
func (rcp *Producer) WithContextsFlag() *Producer {
	rcp.contextsFlag = true

	return rcp
}

// WithInClusterFlag configures the producer to handle an --in-cluster flag, requesting the use
// of a Kubernetes-provided context.
func (rcp *Producer) WithInClusterFlag() *Producer {
	rcp.inClusterFlag = true

	return rcp
}

// SetupFlags configures the given flags to control the producer settings.
func (rcp *Producer) SetupFlags(flags *pflag.FlagSet) {
	if rcp.inClusterFlag {
		flags.BoolVar(&rcp.inCluster, InCluster, false, "use the in-cluster configuration to connect to Kubernetes")
	}

	// The base loading rules are shared across all clientcmd setups.
	// This means that alternative kubeconfig setups (remoteconfig etc.) need to
	// be handled manually, but there's no way around that if we want to allow
	// both --kubeconfig and --remoteconfig (only one set of flags can be configured
	// for a given prefix).
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig

	flags.StringVar(&loadingRules.ExplicitPath, "kubeconfig", "", "absolute path(s) to the kubeconfig file(s)")

	// Default prefix
	rcp.defaultClientConfig = rcp.setupContextFlags(loadingRules, flags, "")

	// Multiple contexts (only on the default prefix)
	if rcp.contextsFlag {
		flags.StringSliceVar(&rcp.contexts, "contexts", nil, "comma-separated list of contexts to use")
	}

	// Other prefixes
	rcp.prefixedClientConfigs = make(map[string]*loadingRulesAndOverrides, len(rcp.contextPrefixes))
	rcp.prefixedKubeConfigs = make(map[string]*string, len(rcp.contextPrefixes))

	for _, prefix := range rcp.contextPrefixes {
		rcp.prefixedKubeConfigs[prefix] = flags.String(prefix+"config", "", "absolute path(s) to the "+prefix+" kubeconfig file(s)")
		rcp.prefixedClientConfigs[prefix] = rcp.setupContextFlags(loadingRules, flags, prefix)
	}
}

func (rcp *Producer) setupContextFlags(
	loadingRules *clientcmd.ClientConfigLoadingRules, flags *pflag.FlagSet, prefix string,
) *loadingRulesAndOverrides {
	// Default un-prefixed context
	overrides := clientcmd.ConfigOverrides{ClusterDefaults: clientcmd.ClusterDefaults}
	kflags := clientcmd.RecommendedConfigOverrideFlags(prefix)

	if !rcp.namespaceFlag {
		// Drop the namespace flag (an empty long name disables a flag)
		kflags.ContextOverrideFlags.Namespace.LongName = ""
	} else if prefix != "" {
		// Avoid attempting to define "-n" twice
		kflags.ContextOverrideFlags.Namespace.ShortName = ""
	}

	clientcmd.BindOverrideFlags(&overrides, flags, kflags)

	return &loadingRulesAndOverrides{
		loadingRules: loadingRules,
		overrides:    &overrides,
	}
}

type AllContextFn func(clusterInfos []*cluster.Info, namespaces []string, status reporter.Interface) error

type PerContextFn func(clusterInfo *cluster.Info, namespace string, status reporter.Interface) error

// RunOnSelectedContext runs the given function on the selected context.
func (rcp *Producer) RunOnSelectedContext(function PerContextFn, status reporter.Interface) error {
	if rcp.inCluster {
		return runInCluster(function, status)
	}

	if rcp.defaultClientConfig == nil {
		// If we get here, no context was set up, which means SetupFlags() wasn't called
		return status.Error(errors.New("no context provided (this is a programming error)"), "")
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rcp.defaultClientConfig.loadingRules, rcp.defaultClientConfig.overrides)

	restConfig, err := getRestConfigFromConfig(clientConfig, rcp.defaultClientConfig.overrides)
	if err != nil {
		return status.Error(err, "error retrieving the default configuration")
	}

	clusterInfo, err := cluster.NewInfo(restConfig.ClusterName, restConfig.Config)
	if err != nil {
		return status.Error(err, "error building the cluster.Info for the default configuration")
	}

	var submVersion string
	if clusterInfo.Submariner != nil {
		submVersion = clusterInfo.Submariner.Spec.Version
	} else if clusterInfo.ServiceDiscovery != nil {
		submVersion = clusterInfo.ServiceDiscovery.Spec.Version
	}

	if submVersion != "" {
		if err = checkVersionMismatch(submVersion); err != nil {
			return status.Error(err, "")
		}
	}

	namespace, overridden, err := clientConfig.Namespace()
	if err != nil {
		return status.Error(err, "error retrieving the namespace for the default configuration")
	}

	if !overridden && rcp.defaultNamespace != nil {
		namespace = *rcp.defaultNamespace
	}

	return function(clusterInfo, namespace, status)
}

func runInCluster(function PerContextFn, status reporter.Interface) error {
	restConfig, err := GetInClusterConfig()
	if err != nil {
		return status.Error(err, "error retrieving the in-cluster configuration")
	}

	// In-cluster configurations don't give a cluster name, use "in-cluster"
	clusterInfo, err := cluster.NewInfo(InCluster, restConfig)
	if err != nil {
		return status.Error(err, "error building the cluster.Info for the in-cluster configuration")
	}

	// In-cluster configurations don't specify a namespace, use the default
	// When using the in-cluster configuration, that's the only configuration we want
	return function(clusterInfo, "", status)
}

// RunOnSelectedPrefixedContext runs the given function on the selected prefixed context.
// Returns true if there was a selected prefix context, false otherwise.
func (rcp *Producer) RunOnSelectedPrefixedContext(prefix string, function PerContextFn, status reporter.Interface) (bool, error) {
	clientConfig, ok := rcp.prefixedClientConfigs[prefix]
	if ok {
		loadingRules := clientConfig.loadingRules

		// If the user specified a kubeconfig for this prefix, use that instead
		contextKubeConfig, ok := rcp.prefixedKubeConfigs[prefix]
		if ok && contextKubeConfig != nil && *contextKubeConfig != "" {
			loadingRules.ExplicitPath = *contextKubeConfig
		}

		// Has the user actually specified a value for the prefixed context?
		if loadingRules.ExplicitPath == "" && areOverridesEmpty(clientConfig.overrides) {
			return false, nil
		}

		contextClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, clientConfig.overrides)

		restConfig, err := getRestConfigFromConfig(contextClientConfig, clientConfig.overrides)
		if err != nil {
			return true, status.Error(err, "error retrieving the configuration for prefix %s", prefix)
		}

		clusterInfo, err := cluster.NewInfo(restConfig.ClusterName, restConfig.Config)
		if err != nil {
			return true, status.Error(err, "error building the cluster.Info for the configuration for prefix %s", prefix)
		}

		namespace, overridden, err := contextClientConfig.Namespace()
		if err != nil {
			return true, status.Error(err, "error retrieving the namespace for the configuration for prefix %s", prefix)
		}

		if !overridden {
			if rcp.prefixedDefaultNamespaces[prefix] != nil {
				namespace = *rcp.prefixedDefaultNamespaces[prefix]
			} else if rcp.defaultNamespace != nil {
				namespace = *rcp.defaultNamespace
			}
		}

		return true, function(clusterInfo, namespace, status)
	}

	return false, nil
}

// RunOnSelectedContexts runs the given function on all selected contexts, passing them simultaneously.
// This specifically handles the "--contexts" (plural) flag.
// Returns true if there was at least one selected context, false otherwise.
func (rcp *Producer) RunOnSelectedContexts(function AllContextFn, status reporter.Interface) (bool, error) {
	if rcp.inCluster {
		return true, runInCluster(func(clusterInfo *cluster.Info, namespace string, status reporter.Interface) error {
			return function([]*cluster.Info{clusterInfo}, []string{namespace}, status)
		}, status)
	}

	if rcp.defaultClientConfig != nil {
		if len(rcp.contexts) > 0 {
			// Loop over explicitly-chosen contexts
			clusterInfos := []*cluster.Info{}
			namespaces := []string{}

			for _, contextName := range rcp.contexts {
				rcp.defaultClientConfig.overrides.CurrentContext = contextName
				clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
					rcp.defaultClientConfig.loadingRules, rcp.defaultClientConfig.overrides)

				restConfig, err := getRestConfigFromConfig(clientConfig, rcp.defaultClientConfig.overrides)
				if err != nil {
					return true, status.Error(err, "error retrieving the configuration for context %s", contextName)
				}

				clusterInfo, err := cluster.NewInfo(restConfig.ClusterName, restConfig.Config)
				if err != nil {
					return true, status.Error(err, "error building the cluster.Info for context %s", contextName)
				}

				clusterInfos = append(clusterInfos, clusterInfo)

				namespace, overridden, err := clientConfig.Namespace()
				if err != nil {
					return true, status.Error(err, "error retrieving the namespace for context %s", contextName)
				}

				if !overridden && rcp.defaultNamespace != nil {
					namespace = *rcp.defaultNamespace
				}

				namespaces = append(namespaces, namespace)
			}

			return true, function(clusterInfos, namespaces, status)
		}
	}

	return false, nil
}

// RunOnAllContexts runs the given function on all accessible non-prefixed contexts.
// If the user has explicitly selected one or more contexts, only those contexts are used.
// All appropriate contexts are processed, and any errors are aggregated.
// Returns an error if no contexts are found.
func (rcp *Producer) RunOnAllContexts(function PerContextFn, status reporter.Interface) error {
	if rcp.inCluster {
		return runInCluster(function, status)
	}

	if rcp.defaultClientConfig == nil {
		// If we get here, no context was set up, which means SetupFlags() wasn't called
		return status.Error(errors.New("no context provided (this is a programming error)"), "")
	}

	if rcp.defaultClientConfig.overrides.CurrentContext != "" {
		// The user has explicitly chosen a context, use that only
		return rcp.RunOnSelectedContext(function, status)
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rcp.defaultClientConfig.loadingRules, rcp.defaultClientConfig.overrides)

	rawConfig, err := clientConfig.RawConfig()
	if err != nil {
		return status.Error(err, "error retrieving the raw kubeconfig setup")
	}

	contextErrors := []error{}
	processedContexts := 0

	if len(rcp.contexts) > 0 {
		// Loop over explicitly-chosen contexts
		for _, contextName := range rcp.contexts {
			processedContexts++

			chosenContext, ok := rawConfig.Contexts[contextName]
			if !ok {
				contextErrors = append(contextErrors, status.Error(fmt.Errorf("no Kubernetes context found named %s", contextName), ""))

				continue
			}

			contextErrors = append(contextErrors, rcp.overrideContextAndRun(chosenContext.Cluster, contextName, function, status))
		}
	} else {
		// Loop over all accessible contexts and de-duplicate by cluster name. If there's multiple contexts for a cluster, bias towards the
		// one whose associated user is associated with the most contexts across all the clusters, with the intent of hopefully picking one
		// that has admin privileges.
		contextsByCluster := map[string][]string{}
		usageByUser := map[string]int{}

		for contextName, context := range rawConfig.Contexts {
			processedContexts++

			contextsByCluster[context.Cluster] = append(contextsByCluster[context.Cluster], contextName)
			usageByUser[context.AuthInfo]++
		}

		for cluster, contextNames := range contextsByCluster {
			if len(contextNames) == 1 {
				contextErrors = append(contextErrors, rcp.overrideContextAndRun(cluster, contextNames[0], function, status))
				continue
			}

			selectedContextName := ""
			maxUsage := 0

			for _, name := range contextNames {
				if usageByUser[rawConfig.Contexts[name].AuthInfo] > maxUsage {
					maxUsage = usageByUser[rawConfig.Contexts[name].AuthInfo]
					selectedContextName = name
				}
			}

			status.Warning("Found multiple kube contexts for cluster %q:\n    %s\n  Context %q was automatically selected however if the"+
				" associated user account does not have sufficient privileges, please re-run the command with the suitable context.\n",
				cluster, strings.Join(contextNames, "\n    "), selectedContextName)

			contextErrors = append(contextErrors, rcp.overrideContextAndRun(cluster, selectedContextName, function, status))
		}
	}

	if processedContexts == 0 {
		return status.Error(errors.New("no Kubernetes configuration or context was found"), "")
	}

	return k8serrors.NewAggregate(contextErrors)
}

func (rcp *Producer) overrideContextAndRun(clusterName, contextName string, function PerContextFn, status reporter.Interface) error {
	fmt.Printf("Cluster %q\n", clusterName)

	rcp.defaultClientConfig.overrides.CurrentContext = contextName
	if err := rcp.RunOnSelectedContext(function, status); err != nil {
		return err
	}

	fmt.Println()

	return nil
}

func ForBroker(submariner *v1alpha1.Submariner, serviceDisc *v1alpha1.ServiceDiscovery) (*rest.Config, string, error) {
	var restConfig *rest.Config
	var namespace string
	var err error

	// This is used in subctl; the broker secret isn't available mounted, so we use the old strings for now
	if submariner != nil {
		// Try to authorize against the submariner Cluster resource as we know the CRD should exist and the credentials
		// should allow read access.
		restConfig, _, err = resource.GetAuthorizedRestConfigFromData(submariner.Spec.BrokerK8sApiServer,
			submariner.Spec.BrokerK8sApiServerToken,
			submariner.Spec.BrokerK8sCA,
			&rest.TLSClientConfig{},
			subv1.SchemeGroupVersion.WithResource("clusters"),
			submariner.Spec.BrokerK8sRemoteNamespace)
		namespace = submariner.Spec.BrokerK8sRemoteNamespace
	} else if serviceDisc != nil {
		// Try to authorize against the ServiceImport resource as we know the CRD should exist and the credentials
		// should allow read access.
		restConfig, _, err = resource.GetAuthorizedRestConfigFromData(serviceDisc.Spec.BrokerK8sApiServer,
			serviceDisc.Spec.BrokerK8sApiServerToken,
			serviceDisc.Spec.BrokerK8sCA,
			&rest.TLSClientConfig{},
			gvr.FromMetaGroupVersion(mcsv1a1.GroupVersion, "serviceimports"),
			serviceDisc.Spec.BrokerK8sRemoteNamespace)
		namespace = serviceDisc.Spec.BrokerK8sRemoteNamespace
	}

	return restConfig, namespace, errors.Wrap(err, "error getting auth rest config")
}

func getRestConfigFromConfig(config clientcmd.ClientConfig, overrides *clientcmd.ConfigOverrides) (RestConfig, error) {
	clientConfig, err := config.ClientConfig()
	if err != nil {
		return RestConfig{}, errors.Wrap(err, "error creating client config")
	}

	raw, err := config.RawConfig()
	if err != nil {
		return RestConfig{}, errors.Wrap(err, "error creating rest config")
	}

	clusterName := clusterNameFromContext(&raw, overrides.CurrentContext)

	if clusterName == nil {
		return RestConfig{}, fmt.Errorf("could not obtain the cluster name from kube config: %#v", raw)
	}

	return RestConfig{Config: clientConfig, ClusterName: *clusterName}, nil
}

func clusterNameFromContext(rawConfig *api.Config, overridesContext string) *string {
	if overridesContext == "" {
		// No context provided, use the current context.
		overridesContext = rawConfig.CurrentContext
	}

	configContext, ok := rawConfig.Contexts[overridesContext]
	if !ok {
		return nil
	}

	return &configContext.Cluster
}

func checkVersionMismatch(submVersion string) error {
	subctlVer, _ := semver.NewVersion(version.Version)
	submarinerVer, _ := semver.NewVersion(submVersion)

	if subctlVer != nil && submarinerVer != nil && subctlVer.LessThan(*submarinerVer) {
		return fmt.Errorf(
			"the subctl version %q is older than the deployed Submariner version %q. Please upgrade your subctl version",
			version.Version, submVersion)
	}

	return nil
}

func IfConnectivityInstalled(functions ...PerContextFn) PerContextFn {
	return func(clusterInfo *cluster.Info, namespace string, status reporter.Interface) error {
		if clusterInfo.Submariner == nil {
			status.Warning(constants.ConnectivityNotInstalled)

			return nil
		}

		aggregateErrors := []error{}

		for _, function := range functions {
			aggregateErrors = append(aggregateErrors, function(clusterInfo, namespace, status))
		}

		return k8serrors.NewAggregate(aggregateErrors)
	}
}

func IfServiceDiscoveryInstalled(functions ...PerContextFn) PerContextFn {
	return func(clusterInfo *cluster.Info, namespace string, status reporter.Interface) error {
		if clusterInfo.ServiceDiscovery == nil {
			status.Warning(constants.ServiceDiscoveryNotInstalled)

			return nil
		}

		aggregateErrors := []error{}

		for _, function := range functions {
			aggregateErrors = append(aggregateErrors, function(clusterInfo, namespace, status))
		}

		return k8serrors.NewAggregate(aggregateErrors)
	}
}

func areOverridesEmpty(overrides *clientcmd.ConfigOverrides) bool {
	return overrides.AuthInfo.ClientCertificate == "" &&
		overrides.AuthInfo.ClientKey == "" &&
		overrides.AuthInfo.Token == "" &&
		overrides.AuthInfo.TokenFile == "" &&
		overrides.AuthInfo.Username == "" &&
		overrides.AuthInfo.Password == "" &&
		overrides.ClusterInfo.CertificateAuthority == "" &&
		overrides.ClusterInfo.ProxyURL == "" &&
		overrides.ClusterInfo.Server == "" &&
		overrides.ClusterInfo.TLSServerName == "" &&
		overrides.CurrentContext == "" &&
		overrides.Context.Cluster == "" &&
		overrides.Context.AuthInfo == "" &&
		overrides.Context.Namespace == ""
}
