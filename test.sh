#!/usr/bin/env bash

# start test node
mkdir ./build/test_sdk

./build/bin/ft --datadir ./build/test_sdk/ft --miner_start > ./build/test_sdk/test.log 2>&1 &


# collect code coverrage data
set -e
echo "mode: count" >coverage.out

for d in $(go list ./... | grep -v vendor | grep -v test); do
    echo testing $d ...
    go test -coverprofile=profile.out -covermode=count $d
    if [ -f profile.out ]; then
        cat profile.out | grep -v "mode: count" | cat >> coverage.out
        rm profile.out
    fi
done

# kill test node 
ps -ef | grep ./build/bin/ft | grep -v grep |  awk -F ' ' '{print $2}' | xargs kill -9

# clear test_sdk data
rm -r ./build/test_sdk

