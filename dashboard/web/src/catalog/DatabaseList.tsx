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
import {
  listMongo,
  listPostgres,
  mongoListKey,
  postgresListKey,
} from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link } from "@/lib/router";

export function DatabaseList() {
  const postgres = useQuery({
    queryKey: postgresListKey,
    queryFn: listPostgres,
    refetchInterval: 10_000,
  });
  const mongo = useQuery({
    queryKey: mongoListKey,
    queryFn: listMongo,
    refetchInterval: 10_000,
  });

  const rows = [
    ...(postgres.data ?? []).map((item) => ({
      key: `postgres:${item.name}`,
      name: item.name,
      engine: `PostgreSQL ${item.spec.version ?? "16"}`,
      storage: item.spec.storageSize ?? "10Gi",
      conditions: item.status.conditions,
      created: item.creationTimestamp,
      href: `/databases/${encodeURIComponent(item.name)}`,
    })),
    ...(mongo.data ?? []).map((item) => ({
      key: `mongo:${item.name}`,
      name: item.name,
      engine: `MongoDB ${item.spec.version ?? "8.0"}`,
      storage: item.spec.storageSize ?? "10Gi",
      conditions: item.status.conditions,
      created: item.creationTimestamp,
      href: `/databases/mongo/${encodeURIComponent(item.name)}`,
    })),
  ].sort((a, b) => a.name.localeCompare(b.name));

  const loading = postgres.isPending || mongo.isPending;
  const loaded = postgres.data !== undefined && mongo.data !== undefined;

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
      {loading ? (
        <p className="font-mono text-xs text-muted-foreground">Loading…</p>
      ) : null}
      <ApiErrorAlert error={postgres.error} />
      <ApiErrorAlert error={mongo.error} />
      {loaded && rows.length === 0 ? (
        <p className="rounded-lg border border-dashed border-primary/50 px-5 py-4 font-mono text-[13px] leading-relaxed text-primary">
          No databases yet — provision PostgreSQL or MongoDB from the catalog.
        </p>
      ) : null}
      {rows.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Engine</TableHead>
              <TableHead>Storage</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Age</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.key}>
                <TableCell>
                  <Link
                    to={row.href}
                    className="font-mono text-primary hover:underline"
                  >
                    {row.name}
                  </Link>
                </TableCell>
                <TableCell className="font-mono text-foreground">
                  {row.engine}
                </TableCell>
                <TableCell className="font-mono text-foreground">
                  {row.storage}
                </TableCell>
                <TableCell>
                  <StatusBadge conditions={row.conditions} />
                </TableCell>
                <TableCell className="font-mono text-muted-foreground">
                  {formatAge(row.created)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : null}
    </section>
  );
}
