#!/bin/bash

set -x
# Find the directory we exist within
DIR=$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )
cd ${DIR}/..

GITVERSION=`git describe --abbrev=7`

docker build -f scripts/Dockerfile -t grafana/metrictank:$GITVERSION .
docker tag grafana/metrictank:$GITVERSION grafana/metrictank:latest

# regular image
#rm -rf build/*
#mkdir -p build
#cp ../build/* build/

#docker build -t grafana/metrictank .
#docker tag grafana/metrictank grafana/metrictank:latest
#docker tag grafana/metrictank grafana/metrictank:$VERSION

# k8s image
#cd ${DIR}/k8s
#docker build -t us.gcr.io/metrictank-gcr/metrictank .
#docker tag us.gcr.io/metrictank-gcr/metrictank us.gcr.io/metrictank-gcr/metrictank:latest
#docker tag us.gcr.io/metrictank-gcr/metrictank us.gcr.io/metrictank-gcr/metrictank:${VERSION}
