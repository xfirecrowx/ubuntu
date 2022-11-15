#!/usr/bin/env bash

set -e

# rm -rf build/_workspace/hera/0.3.2
# mkdir -p build/_workspace/hera/0.3.2
# wget -P build/_workspace/hera/ https://github.com/ewasm/hera/releases/download/v0.3.2/hera-0.3.2-linux-x86_64.tar.gz
# tar -C build/_workspace/hera/0.3.2 -xzf build/_workspace/hera/hera-0.3.2-linux-x86_64.tar.gz

_OSTYPE=${OSTYPE}
if [[ ${_OSTYPE} == "linux"* ]]; then
    _OSTYPE="linux"
fi
if [[ ${_OSTYPE} == "darwin"* ]]; then
    _OSTYPE="darwin"
fi

main() {
    mkdir -p build/_workspace
    [ ! -d build/_workspace/hera ] && git clone https://github.com/ewasm/hera build/_workspace/hera || echo "Hera exists."
    cd build/_workspace/hera
    git checkout v0.6.0
    git submodule update --init
    mkdir -p build
    cd build
    cmake -DBUILD_SHARED_LIBS=ON ..
    cmake --build .
    echo "Built library at: $(pwd)/src/libhera.so"
}
main
