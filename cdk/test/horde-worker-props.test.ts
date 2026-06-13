import { RemovalPolicy } from "aws-cdk-lib";
import type { DataRemovalPolicy, HordeNetworkMode, HordeWorkerProps } from "../src";

describe("HordeWorkerProps", () => {
  it("re-exports from index (compile-time check)", () => {
    const _props: HordeWorkerProps | undefined = undefined;
    expect(_props).toBeUndefined();
  });

  it("requires projectSlug", () => {
    // @ts-expect-error – projectSlug is required
    const _bad: HordeWorkerProps = {
      workerImage: {} as never,
      ecrRepository: {} as never,
      secrets: {} as never,
    };
    expect(_bad).toBeDefined();
  });

  it("accepts networkMode 'public' and 'private'", () => {
    const a: HordeNetworkMode = "public";
    const b: HordeNetworkMode = "private";
    expect([a, b]).toEqual(["public", "private"]);
  });

  it("accepts RETAIN and DESTROY as DataRemovalPolicy", () => {
    const a: DataRemovalPolicy = RemovalPolicy.RETAIN;
    const b: DataRemovalPolicy = RemovalPolicy.DESTROY;
    expect([a, b]).toEqual([RemovalPolicy.RETAIN, RemovalPolicy.DESTROY]);
  });

  it("rejects SNAPSHOT as DataRemovalPolicy (compile-time)", () => {
    // @ts-expect-error – SNAPSHOT is not a valid DataRemovalPolicy
    const bad: DataRemovalPolicy = RemovalPolicy.SNAPSHOT;
    expect(bad).toBeDefined();
  });

  it("rejects RETAIN_ON_UPDATE_OR_DELETE as DataRemovalPolicy (compile-time)", () => {
    // @ts-expect-error – only RETAIN and DESTROY are valid DataRemovalPolicy values
    const bad: DataRemovalPolicy = RemovalPolicy.RETAIN_ON_UPDATE_OR_DELETE;
    expect(bad).toBeDefined();
  });

  it("accepts dataRemovalPolicy and pointInTimeRecovery props", () => {
    const partial: Pick<HordeWorkerProps, "dataRemovalPolicy" | "pointInTimeRecovery"> = {
      dataRemovalPolicy: RemovalPolicy.DESTROY,
      pointInTimeRecovery: false,
    };
    expect(partial.dataRemovalPolicy).toBe(RemovalPolicy.DESTROY);
    expect(partial.pointInTimeRecovery).toBe(false);
  });
});
