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
import { listPostgres, postgresListKey } from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link } from "@/lib/router";

export function PostgresList() {
  const query = useQuery({
    queryKey: postgresListKey,
    queryFn: listPostgres,
    refetchInterval: 10_000,
  });

  return (
    <section className="flex flex-col gap-6">
      <div className="flex items-center justify-between">
        <h1 className="font-display text-2xl font-medium tracking-tight text-white">
          Databases
        </h1>
        <Button asChild>
          <Link to="/databases/new">New database</Link>
        </Button>
      </div>
      {query.isPending && (
        <p className="font-mono text-xs text-muted-foreground">Loading…</p>
      )}
      <ApiErrorAlert error={query.error} />
      {query.data &&
        (query.data.length === 0 ? (
          <p className="rounded-lg border border-dashed border-primary/50 px-5 py-4 font-mono text-[13px] leading-relaxed text-primary">
            No databases yet — provision a PostgreSQL instance from the
            catalog.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Storage</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Age</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {query.data.map((pg) => (
                <TableRow key={pg.name}>
                  <TableCell>
                    <Link
                      to={`/databases/${encodeURIComponent(pg.name)}`}
                      className="font-mono text-primary hover:underline"
                    >
                      {pg.name}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono text-foreground">
                    PostgreSQL {pg.spec.version ?? "16"}
                  </TableCell>
                  <TableCell className="font-mono text-foreground">
                    {pg.spec.storageSize ?? "10Gi"}
                  </TableCell>
                  <TableCell>
                    <StatusBadge conditions={pg.status.conditions} />
                  </TableCell>
                  <TableCell className="font-mono text-muted-foreground">
                    {formatAge(pg.creationTimestamp)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ))}
    </section>
  );
}
