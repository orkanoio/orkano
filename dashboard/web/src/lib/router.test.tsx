import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { navigate, routeSegments, useRoute } from "@/lib/router";

function ShowRoute() {
  const route = useRoute();
  return <p>route:{route}</p>;
}

describe("hash router", () => {
  it("re-renders on hash navigation", async () => {
    render(<ShowRoute />);
    expect(screen.getByText("route:/")).toBeInTheDocument();

    navigate("/apps/web");
    await waitFor(() => {
      expect(screen.getByText("route:/apps/web")).toBeInTheDocument();
    });
  });

  it("splits and decodes route segments", () => {
    expect(routeSegments("/")).toEqual([]);
    expect(routeSegments("/apps/web/edit")).toEqual(["apps", "web", "edit"]);
    expect(routeSegments("/apps/my%20app")).toEqual(["apps", "my app"]);
  });
});
