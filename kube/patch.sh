#!/bin/sh

set -e
set -u

usage() {
  echo -e "Usage: $0 <deployment|statefulset> <name>\n"
}

if [  $# -le 1 ]; then
  usage
  exit 1
fi

# Override to use a different Docker image name for the sidecar.
export SIDECAR_IMAGE_NAME=${SIDECAR_IMAGE_NAME:-'gcr.io/stackdriver-prometheus/stackdriver-prometheus-sidecar'}

kubectl -n "${KUBE_NAMESPACE}" patch "$1" "$2" --type strategic --patch "
spec:
  template:
    spec:
      containers:
      - name: sidecar
        image: ${SIDECAR_IMAGE_NAME}:${SIDECAR_IMAGE_TAG}
        imagePullPolicy: Always
        args:
        - \"--stackdriver.project-id=${GCP_PROJECT}\"
        - \"--prometheus.wal-directory=${DATA_DIR}/wal\"
        - \"--stackdriver.kubernetes.location=${GCP_REGION}\"
        - \"--stackdriver.kubernetes.cluster-name=${KUBE_CLUSTER}\"
        - \"--include=kube_deployment_status_replicas_available\"
        - \"--include=python_gc_objects_collected_total\"
        - \"--include=python_gc_objects_uncollectable_total\"
        - \"--include=python_gc_collections_total\"
        - \"--include=python_info\"
        - \"--include=process_virtual_memory_bytes\"
        - \"--include=process_resident_memory_bytes\"
        - \"--include=process_start_time_seconds\"
        - \"--include=process_cpu_seconds_total\"
        - \"--include=process_open_fds\"
        - \"--include=process_max_fds\"
        - \"--include=flask_http_request_duration_seconds_bucket\"
        - \"--include=flask_http_request_duration_seconds_count\"
        - \"--include=flask_http_request_duration_seconds_sum\"
        - \"--include=flask_http_request_duration_seconds_created\"
        - \"--include=flask_http_request_total\"
        - \"--include=flask_http_request_created\"
        - \"--include=flask_exporter_info\"
        - \"--include=app_info\"
        #- \"--stackdriver.generic.location=${GCP_REGION}\"
        #- \"--stackdriver.generic.namespace=${KUBE_CLUSTER}\"
        ports:
        - name: sidecar
          containerPort: 9091
        volumeMounts:
        - name: ${DATA_VOLUME}
          mountPath: ${DATA_DIR}
"


