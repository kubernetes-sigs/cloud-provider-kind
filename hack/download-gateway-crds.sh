#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
REPO_ROOT=$( cd -- "$SCRIPT_DIR/.." &> /dev/null && pwd )

repo_url="https://github.com/kubernetes-sigs/gateway-api"

# Define the desired git version (tag or branch). Default to 'main'.
# Can be overridden by setting the GIT_VERSION environment variable.
GIT_VERSION="${GIT_VERSION:-main}"

# Define the path within the repo where CRDs are located
# Using 'standard' as it's common, adjust if needed for different versions
crd_repo_subdir="config/crd"

# Define the output directory relative to the REPO_ROOT
output_dir_rel="pkg/gateway/crds"
output_dir_abs="$REPO_ROOT/$output_dir_rel"

# --- Temporary Directory and Cleanup ---
TEMP_CLONE_DIR="" # Will hold the path to the temporary directory

cleanup() {
  local exit_code=$? # Capture exit code
  echo "Cleaning up..."
  if [ -n "$TEMP_CLONE_DIR" ] && [ -d "$TEMP_CLONE_DIR" ]; then
    echo "Removing temporary clone directory $TEMP_CLONE_DIR..."
    rm -rf "$TEMP_CLONE_DIR"
  else
      echo "No temporary directory to remove."
  fi
  # Ensure we exit with the original script's exit code
  exit $exit_code
}

# Set trap for cleanup on EXIT, ERR, SIGINT, SIGTERM
trap cleanup EXIT ERR SIGINT SIGTERM

# Create a temporary directory for the clone
TEMP_CLONE_DIR=$(mktemp -d -t gateway-api-clone-XXXXXX)
# Define the actual path where the repo will be cloned inside the temp dir
repo_local_path="$TEMP_CLONE_DIR/gateway-api"

echo "Cloning $repo_url (version: $GIT_VERSION) into temporary directory $repo_local_path..."
# Use --depth 1 for a shallow clone.
# --branch works for tags and branches.
# Use --no-tags to potentially speed up if tags aren't needed beyond the specific version tag.
# Add --filter=blob:none if supported by git version and remote to further reduce clone size.
git clone --branch "$GIT_VERSION" --depth 1 "$repo_url" "$repo_local_path"
if [ $? -ne 0 ]; then
    echo "Error: Failed to clone repository for version '$GIT_VERSION'."
    # Cleanup function will handle removing TEMP_CLONE_DIR via trap
    exit 1
fi
echo "Successfully cloned version '$GIT_VERSION'."

# Create the output directory if it doesn't exist
mkdir -p "$output_dir_abs"

# Define the full path to the CRD source directory within the clone
crd_source_dir="$repo_local_path/$crd_repo_subdir"

# Check if the source directory exists
if [ ! -d "$crd_source_dir" ]; then
    echo "Error: CRD source directory not found: $crd_source_dir (for version: $GIT_VERSION)"
    exit 1
fi

echo "Copying files from: $crd_source_dir (Version: $GIT_VERSION)"

cp -rf "$crd_source_dir"/* "$output_dir_abs"

echo "Successfully copied files to the '$output_dir_abs' directory from version $GIT_VERSION."

# Cleanup is handled by the trap
exit 0
