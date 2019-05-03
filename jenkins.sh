#!/bin/bash

set -ex

PKG=github.com/loomnetwork/loomchain

# setup temp GOPATH
export GOPATH=/tmp/gopath-$BUILD_TAG
export
export PATH=$GOPATH:$PATH:/var/lib/jenkins/workspace/commongopath/bin:$GOPATH/bin

LOOM_SRC=$GOPATH/src/$PKG
mkdir -p $LOOM_SRC
rsync -r --delete . $LOOM_SRC

if [[ "$OSTYPE" == "linux-gnu" ]]; then
export CGO_CFLAGS="-I/usr/local/include/leveldb"
export CGO_LDFLAGS="-L/usr/local/lib/ -L/usr/lib/x86_64-linux-gnu/ -lsnappy"
#elif [[ "$OSTYPE" == "darwin"* ]]; then #osx
fi

cd $LOOM_SRC
make clean
make get_lint
make deps
make lint || true
make linterrors
make  # on OSX we don't need any C precompiles like cleveldb
make validators-tool
make tgoracle
make loomcoin_tgoracle
make dposv2_oracle
make plasmachain

##TODO reenable
#make loom-cleveldb
#make plasmachain-cleveldb


export LOOM_BIN=`pwd`/loom
export LOOM_VALIDATORS_TOOL=`pwd`/e2e/validators-tool

export GORACE="log_path=`pwd`/racelog"
#make loom-race
#make test-race

##TODO REENABLE
##make test

#make test-no-evm
make no-evm-tests
make test-app-store-race

#setup & run truffle tests
#cd e2e/tests/truffle
#yarn

#cd ../receipts
#bash ./run_truffle_tests.sh
