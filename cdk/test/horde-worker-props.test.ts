import type { HordeWorkerProps } from "../src";

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
});
