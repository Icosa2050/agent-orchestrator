import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { UpdatesSection } from "./UpdatesSection";
import { BillingSection } from "./BillingSection";
import { aoBridge } from "../../lib/bridge";

vi.mock("../../hooks/useCloudGate", () => ({ useCloudGate: () => ({ cloudEnabled: true }) }));
vi.mock("../../hooks/useCloudCp", () => ({ useCloudCp: () => { throw new Error("local billing must not initialize hosted client"); } }));

it("shows locally managed updates without reading updater settings", () => {
 const get = vi.spyOn(aoBridge.updateSettings, "get");
 render(<UpdatesSection />);
 expect(screen.getByText("Updates managed locally")).toBeInTheDocument();
 expect(screen.queryByLabelText("Updates channel")).not.toBeInTheDocument();
 expect(get).not.toHaveBeenCalled();
 get.mockRestore();
});

it("gates billing even when persisted cloud settings are enabled", () => {
 render(<BillingSection />);
 expect(screen.getByText("Cloud billing unavailable in this local build")).toBeInTheDocument();
 expect(screen.queryByText("Starter")).not.toBeInTheDocument();
});
