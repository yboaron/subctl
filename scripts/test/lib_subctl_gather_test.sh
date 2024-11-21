#!/bin/bash
# shellcheck disable=SC2154

# This should only be sourced
if [ "${0##*/}" = "lib_subctl_gather_test.sh" ]; then
    echo "Don't run me, source me" >&2
    exit 1
fi

gather_out_dir=/tmp/subctl-gather-output

function validate_gathered_files () {

  # connectivity
  validate_resource_files "$subm_ns" 'endpoints.submariner.io' 'Endpoint'
  validate_resource_files "$subm_ns" 'clusters.submariner.io' 'Cluster'
  validate_resource_files "$subm_ns" 'gateways.submariner.io' 'Gateway'
  validate_resource_files all 'clusterglobalegressips.submariner.io' 'ClusterGlobalEgressIP'
  validate_resource_files all 'globalegressips.submariner.io' 'GlobalEgressIP'
  validate_resource_files all 'globalingressips.submariner.io' 'GlobalIngressIP'

  validate_pod_log_files "$subm_ns" '-l app=submariner-gateway' 'submariner-gateway submariner-gateway-init'
  validate_pod_log_files "$subm_ns" '-l app=submariner-routeagent' 'submariner-routeagent submariner-routeagent-init'
  validate_pod_log_files "$subm_ns" '-l app=submariner-globalnet'
  validate_pod_log_files "$subm_ns" '-l app=submariner-networkplugin-syncer'

  # operator
  validate_resource_files "$subm_ns" 'submariners' 'Submariner'
  validate_resource_files "$subm_ns" 'servicediscoveries' 'ServiceDiscovery'
  validate_resource_files "$subm_ns" 'daemonsets' 'DaemonSet' -l app=submariner-gateway
  validate_resource_files "$subm_ns" 'daemonsets' 'DaemonSet' -l app=submariner-routeagent
  validate_resource_files "$subm_ns" 'daemonsets' 'DaemonSet' -l app=submariner-globalnet
  validate_resource_files "$subm_ns" 'deployments' 'Deployment' -l app=submariner-networkplugin-syncer
  validate_resource_files "$subm_ns" 'deployments' 'Deployment' -l app=submariner-lighthouse-agent
  validate_resource_files "$subm_ns" 'deployments' 'Deployment' -l app=submariner-lighthouse-coredns
  validate_resource_files "$subm_ns" 'deployments' 'Deployment' --field-selector metadata.name=submariner-operator

  validate_pod_log_files "$subm_ns" '-l name=submariner-operator'

  # Service Discovery
  if [[ "$LIGHTHOUSE" == "true" ]]; then
    validate_resource_files all 'serviceexports.multicluster.x-k8s.io' 'ServiceExport'
    validate_resource_files all 'serviceimports.multicluster.x-k8s.io' 'ServiceImport'
    validate_resource_files all 'endpointslices.discovery.k8s.io' 'EndpointSlice' -l endpointslice.kubernetes.io/managed-by=lighthouse-agent.submariner.io
    validate_resource_files "$subm_ns" 'configmaps' 'ConfigMap' -l component=submariner-lighthouse
    validate_resource_files kube-system 'configmaps' 'ConfigMap' --field-selector metadata.name=coredns

    validate_pod_log_files "$subm_ns" '-l component=submariner-lighthouse'
    validate_pod_log_files kube-system '-l k8s-app=kube-dns'
  fi
}

function validate_pod_log_files() {
  local ns=$1
  local selector=$2
  local containers=$3
  local nsarg="--namespace=${ns}"

  if [[ "$ns" == "all" ]]; then
    nsarg="-A"
  fi
  pod_names=$(kubectl get pods "$nsarg" "$selector" -o=jsonpath='{.items..metadata.name}')
  read -ra pod_names_array <<< "$pod_names"

  read -ra containers_array <<< "$containers"

  for pod_name in "${pod_names_array[@]}"; do
    if [[ "${#containers_array[@]}" == 0 ]]; then
        file=$gather_out_dir/${cluster}/$pod_name.log
        cat "$file"
    else
      for container in "${containers_array[@]}"; do
        file=$gather_out_dir/${cluster}/$pod_name-$container.log
        cat "$file"
      done
    fi
  done
}

function validate_resource_files() {
  local ns=$1
  local resource=$2
  local kind=$3
  local selector=("${@:4}")

  local kubectl_flags=(-o json)
  [[ "$ns" == "all" ]] && kubectl_flags+=(-A) || kubectl_flags+=("--namespace=${ns}")
  [[ "${#selector[@]}" -eq 0 ]] || kubectl_flags+=("${selector[@]}")

  json=$(kubectl get "$resource" "${kubectl_flags[@]}")
  names=$(jq .items[].metadata.name <<< "$json")
  names=$(echo "$names" | tr '\n' ' ' | tr -d '"')
  namespaces=$(jq .items[].metadata.namespace <<< "$json")
  namespaces=$(echo "$namespaces" | tr '\n' ' ' | tr -d '"' | sed 's/null//g')
  read -ra names_array <<< "$names"
  read -ra namespaces_array <<< "$namespaces"

  short_res=$(echo "$resource" | awk -F. '{ print $1 }')

  for i in "${!names_array[@]}"; do
    name=${names_array[$i]}
    namespace=${namespaces_array[$i]}
    file=$gather_out_dir/${cluster}/${short_res}_${namespace}_${name}.yaml
    cat "$file"

    kind_count=$(grep -c "kind: $kind$" "$file")
    if [[ $kind_count != "1" ]]; then
      echo "Expected 1 kind: $kind"
      return 1
    fi

    res_name=$(grep "name: $name$" "$file")
    if [[ $res_name == "" ]]; then
      echo "Expected resource name: $name"
      return 1
    fi
  done
}

function validate_broker_resources() {
  validate_resource_files "$submariner_broker_ns" endpoints.submariner.io Endpoint
  validate_resource_files "$submariner_broker_ns" clusters.submariner.io Cluster
  validate_resource_files "$submariner_broker_ns" serviceimports.multicluster.x-k8s.io ServiceImport
  validate_resource_files "$submariner_broker_ns" endpointslices.discovery.k8s.io EndpointSlice
}
