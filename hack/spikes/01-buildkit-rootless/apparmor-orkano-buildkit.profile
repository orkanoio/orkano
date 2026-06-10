# AppArmor profile for Orkano build pods (spike version — deliberately broad).
# Loaded on each node, referenced from the Job via
# securityContext.appArmorProfile: {type: Localhost, localhostProfile: orkano-buildkit}.
# Localhost profiles are admittable under PSA *baseline*; Unconfined is not.
# The two rules the cri-containerd default profile lacks and rootless BuildKit
# needs are `userns,` and `mount,` (the default profile carries `deny mount,`,
# which fails silently — no audit log entry).
# Install: sudo cp apparmor-orkano-buildkit.profile /etc/apparmor.d/orkano-buildkit
#          sudo apparmor_parser -r /etc/apparmor.d/orkano-buildkit
abi <abi/4.0>,
include <tunables/global>

profile orkano-buildkit flags=(attach_disconnected,mediate_deleted) {
  userns,
  capability,
  network,
  mount,
  umount,
  remount,
  pivot_root,
  signal,
  ptrace,
  unix,
  mqueue,
  file,
}
