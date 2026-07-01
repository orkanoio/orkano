import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { StatusBadge } from "@/components/StatusBadge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { appsKey, listApps } from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link } from "@/lib/router";

export function AppList() {
  const query = useQuery({
    queryKey: appsKey,
    queryFn: listApps,
    refetchInterval: 10_000,
  });

  return (
    <section className="flex flex-col gap-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">Apps</h1>
        <Button asChild>
          <Link to="/apps/new">New app</Link>
        </Button>
      </div>
      {query.isPending && (
        <p className="text-muted-foreground text-sm">Loading…</p>
      )}
      <ApiErrorAlert error={query.error} />
      {query.data &&
        (query.data.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            No apps yet — create one to deploy a repository.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>URL</TableHead>
                <TableHead>Replicas</TableHead>
                <TableHead>Age</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {query.data.map((app) => (
                <TableRow key={app.name}>
                  <TableCell>
                    <Link
                      to={`/apps/${encodeURIComponent(app.name)}`}
                      className="text-primary font-medium hover:underline"
                    >
                      {app.name}
                    </Link>
                  </TableCell>
                  <TableCell>{app.spec.type ?? "Web"}</TableCell>
                  <TableCell>
                    <StatusBadge conditions={app.status.conditions} />
                  </TableCell>
                  <TableCell>
                    {app.status.url ? (
                      <a
                        href={app.status.url}
                        target="_blank"
                        rel="noreferrer"
                        className="text-primary hover:underline"
                      >
                        {app.status.url}
                      </a>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell>
                    {(app.status.availableReplicas ?? 0).toString()}/
                    {(app.spec.replicas ?? 1).toString()}
                  </TableCell>
                  <TableCell>{formatAge(app.creationTimestamp)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ))}
    </section>
  );
}
