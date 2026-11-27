#!/usr/bin/env bash
# Install the modules from build.sh on a stick: ./install.sh <ssh-host>
# They go to /lib/modules/<release>/updates (kept apart from the stock ones) and load on demand by USB ID.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
H=${1:?usage: ./install.sh <ssh-host>}
REL=6.6.0-msm8916
[ "$(ssh "$H" uname -r)" = "$REL" ] || { echo "$H does not run $REL"; exit 1; }
ls "$HERE/out/$REL"/*.ko >/dev/null
T=$(ssh "$H" mktemp -d)
scp -q -r "$HERE/out/$REL" "$HERE/out/firmware" "$H:$T/"
ssh "$H" "sudo sh -euc 'install -d /lib/modules/$REL/updates /lib/firmware/rtl_nic
install -m 644 $T/$REL/*.ko /lib/modules/$REL/updates/
install -m 644 $T/firmware/rtl_nic/*.fw /lib/firmware/rtl_nic/
depmod -a $REL; rm -rf $T'"
echo "installed on $H: $(cd "$HERE/out/$REL" && ls *.ko | tr '\n' ' ')"
