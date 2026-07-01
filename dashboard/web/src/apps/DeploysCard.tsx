import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
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
import { appDeploysKey, listAppDeploys } from "@/lib/api";
import { formatAge } from "@/lib/format";

// DeploysCard shows the dashboard-recorded deploy timeline (created/updated
// rows; the operator owns rollout truth — the live image is on App.status).
export function DeploysCard({ appName }: { appName: string }) {
  const query = useQuery({
    queryKey: appDeploysKey(appName),
    queryFn: () => listAppDeploys(appName),
    refetchInterval: 10_000,
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Deploys</CardTitle>
        <CardDescription>
          Changes made through the dashboard, newest first.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <ApiErrorAlert error={query.error} />
        {query.data &&
          (query.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">
              No deploys recorded yet.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Change</TableHead>
                  <TableHead>Build</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {query.data.map((d, i) => (
                  <TableRow key={i.toString()}>
                    <TableCell title={d.occurredAt}>
                      {formatAge(d.occurredAt)}
                    </TableCell>
                    <TableCell>{d.status}</TableCell>
                    <TableCell className="font-mono text-xs">
                      {d.buildName ?? "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ))}
      </CardContent>
    </Card>
  );
}
