#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_FILE="${ROOT_DIR}/helm/opennebula-csi/Chart.yaml"
VALUES_FILE="${ROOT_DIR}/helm/opennebula-csi/values.yaml"
EXPECTED_TAG="${1:-}"

chart_version="$(awk -F': ' '$1=="version" {print $2; exit}' "${CHART_FILE}")"
chart_app_version="$(awk -F': ' '$1=="appVersion" {gsub(/"/, "", $2); print $2; exit}' "${CHART_FILE}")"
image_tag="$(
  sed -n '/^image:/,/^[^[:space:]]/p' "${VALUES_FILE}" \
    | awk -F': ' '/^[[:space:]]+tag:/ {gsub(/"/, "", $2); print $2; exit}'
)"

if [[ -z "${chart_version}" || -z "${chart_app_version}" || -z "${image_tag}" ]]; then
  echo "failed to resolve chart/version fields from ${CHART_FILE} and ${VALUES_FILE}" >&2
  exit 1
fi

if [[ "${chart_app_version}" != "${image_tag}" ]]; then
  echo "chart appVersion (${chart_app_version}) must match values image.tag (${image_tag})" >&2
  exit 1
fi

if [[ -n "${EXPECTED_TAG}" ]]; then
  expected_chart_version="${EXPECTED_TAG#v}"
  if [[ "${chart_app_version}" != "${EXPECTED_TAG}" ]]; then
    echo "chart appVersion (${chart_app_version}) must match release tag (${EXPECTED_TAG})" >&2
    exit 1
  fi
  if [[ "${image_tag}" != "${EXPECTED_TAG}" ]]; then
    echo "values image.tag (${image_tag}) must match release tag (${EXPECTED_TAG})" >&2
    exit 1
  fi
  if [[ "${chart_version}" != "${expected_chart_version}" ]]; then
    echo "chart version (${chart_version}) must match release tag version (${expected_chart_version})" >&2
    exit 1
  fi
fi

echo "chart version alignment OK: version=${chart_version}, appVersion=${chart_app_version}, image.tag=${image_tag}"
