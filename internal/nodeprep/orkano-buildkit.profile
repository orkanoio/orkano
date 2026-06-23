# AppArmor profile for Orkano build pods (ADR-0012). Referenced from build Jobs
# via securityContext.appArmorProfile: {type: Localhost, localhostProfile: orkano-buildkit}.
# Localhost profiles are admittable under PSA *baseline*; Unconfined is not.
# The two rules the cri-containerd default profile lacks and rootless BuildKit
# needs are `userns,` and `mount,` (the default profile carries `deny mount,`,
# which fails silently — no audit log entry).
# Must be loaded on every node BEFORE a build pod schedules:
#   sudo cp orkano-buildkit.profile /etc/apparmor.d/orkano-buildkit
#   sudo apparmor_parser -r /etc/apparmor.d/orkano-buildkit
# Consumers: hack/ci/substrate-smoke (CI), `orkano init` node prep (M1.5), the
# build.apparmor-profile-loaded doctor check. The copy under hack/spikes/ is the
# frozen spike artifact; this file is the live one.
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
