import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { CommandLine } from "@/components/CommandLine";
import { StatusDot } from "@/components/StatusBadge";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { listNodes, nodesKey, type NodeInfo } from "@/lib/api";
import { formatAge } from "@/lib/format";

// The supported HA shape: one server, or three servers for etcd quorum. The
// helper renders the exact re-run — orkano init converges, it never
// reinstalls the existing server.
const addNodeCommand = `orkano init \\
  --node <existing-server> --node <new-node-1> --node <new-node-2> \\
  --ssh-user root --ssh-key ~/.ssh/id_ed25519 --accept-new-host-key \\
  ...your original flags...`;

export function SettingsPage() {
  const query = useQuery({
    queryKey: nodesKey,
    queryFn: listNodes,
    refetchInterval: 10_000,
  });

  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h1 className="font-display text-3xl font-medium tracking-tight text-white">
          Settings
        </h1>
        {query.data ? (
          <p className="font-mono text-xs text-muted-foreground">
            {query.data.length.toString()}{" "}
            {query.data.length === 1 ? "node" : "nodes"}
          </p>
        ) : null}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Cluster nodes</CardTitle>
          <CardDescription>
            Every node this installation runs on, read live from the cluster.
            The dashboard can only look — nodes are added and removed outside
            it.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {query.isPending ? (
            <p className="font-mono text-xs text-muted-foreground">Loading…</p>
          ) : null}
          <ApiErrorAlert error={query.error} />
          {query.data ? (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Roles</TableHead>
                    <TableHead>Version</TableHead>
                    <TableHead>Internal IP</TableHead>
                    <TableHead>Age</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {query.data.map((node) => (
                    <NodeRow key={node.name} node={node} />
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Add a node</CardTitle>
          <CardDescription>
            The dashboard holds no SSH reach into your machines — growing the
            cluster is a CLI operation run from your workstation.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <p className="text-sm leading-relaxed text-muted-foreground">
            A single-server install grows to the supported high-availability
            shape by re-running{" "}
            <code className="font-mono text-foreground">orkano init</code>{" "}
            with three servers — the existing one listed{" "}
            <span className="text-foreground">first</span>, plus two fresh
            machines:
          </p>
          <CommandLine command={addNodeCommand} />
          <ul className="flex list-disc flex-col gap-1.5 pl-5 text-sm leading-relaxed text-muted-foreground">
            <li>
              Servers come in odd counts — one or three — so embedded etcd
              keeps quorum. Two servers is worse than one.
            </li>
            <li>
              The re-run converges: the existing server is verified, never
              reinstalled, and the new nodes join its cluster.
            </li>
            <li>
              Every node needs an AppArmor-capable OS (Ubuntu 24.04 LTS
              recommended) — the installer loads and verifies the build
              confinement profile on each one, and refuses a node where it
              cannot.
            </li>
            <li>
              On a bring-your-own cluster, add nodes through your platform
              instead, then re-run{" "}
              <code className="font-mono text-foreground">
                orkano preflight
              </code>{" "}
              to confirm the new nodes carry the required capabilities.
            </li>
          </ul>
        </CardContent>
      </Card>
    </section>
  );
}

function NodeRow({ node }: { node: NodeInfo }) {
  return (
    <TableRow>
      <TableCell className="font-mono text-[13px] text-foreground">
        {node.name}
      </TableCell>
      <TableCell>
        <Badge
          variant={
            node.ready ? "success" : node.status === "NotReady" ? "destructive" : "warning"
          }
        >
          <StatusDot />
          {node.status}
          {node.unschedulable ? " · cordoned" : ""}
        </Badge>
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {node.roles.join(", ")}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {node.kubeletVersion}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {node.internalIP}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {formatAge(node.creationTimestamp)}
      </TableCell>
    </TableRow>
  );
}
