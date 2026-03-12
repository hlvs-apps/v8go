#!/bin/bash

echo "=== System Information ==="
echo "Architecture: $(uname -m)"
echo "GCC target: $(gcc -dumpmachine 2>/dev/null || echo 'GCC not found')"
echo "Linker version: $(ld --version | head -1)"

echo -e "\n=== Library Directory ==="
ls -la /workspaces/v8go/deps/linux_amd64/

echo -e "\n=== Library File Types ==="
for lib in /workspaces/v8go/deps/linux_amd64/libv8*.a; do
    if [ -f "$lib" ]; then
        echo "$(basename $lib): $(file $lib)"
    fi
done

echo -e "\n=== Library Manifest ==="
if [ -f /workspaces/v8go/deps/linux_amd64/libmanifest ]; then
    cat /workspaces/v8go/deps/linux_amd64/libmanifest
else
    echo "No libmanifest found"
fi

echo -e "\n=== Sample Object Files ==="
cd /tmp
for lib in /workspaces/v8go/deps/linux_amd64/libv8*.a; do
    if [ -f "$lib" ]; then
        echo "Checking $(basename $lib):"
        ar -x "$lib" 2>/dev/null
        if ls *.o &>/dev/null; then
            file *.o | head -2
            rm -f *.o
        else
            echo "  No object files extracted or archive is empty/corrupted"
        fi
        echo
    fi
done