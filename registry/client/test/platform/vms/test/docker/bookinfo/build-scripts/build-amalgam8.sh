#!/bin/bash
#
# Copyright 2016 IBM Corporation
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.


set -x

SCRIPTDIR=$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )

$SCRIPTDIR/build-sidecar.sh
if [ $? -ne 0 ]; then
    echo "Sidecar failed to compile"
    exit 1
fi
$SCRIPTDIR/build-registry.sh
if [ $? -ne 0 ]; then
    echo "Registry failed to compile"
    exit 1
fi
$SCRIPTDIR/build-controller.sh
if [ $? -ne 0 ]; then
    echo "Controller failed to compile"
    exit 1
fi
$SCRIPTDIR/build-apps.sh
if [ $? -ne 0 ]; then
    echo "Failed to build apps images"
    exit 1
fi
