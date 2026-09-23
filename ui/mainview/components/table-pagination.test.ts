import { describe, expect, test } from "bun:test";
import { pageItems } from "./table-pagination";

describe("pageItems", () => {
  test("lists every page when they all fit", () => {
    expect(pageItems(1, 1)).toEqual([1]);
    expect(pageItems(3, 7)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  test("collapses the far end near the start", () => {
    expect(pageItems(1, 20)).toEqual([1, 2, 3, 4, 5, "ellipsis-end", 20]);
    expect(pageItems(4, 20)).toEqual([1, 2, 3, 4, 5, "ellipsis-end", 20]);
  });

  test("collapses the near end close to the last page", () => {
    expect(pageItems(20, 20)).toEqual([1, "ellipsis-start", 16, 17, 18, 19, 20]);
    expect(pageItems(17, 20)).toEqual([1, "ellipsis-start", 16, 17, 18, 19, 20]);
  });

  test("centres the current page between two ellipses", () => {
    expect(pageItems(10, 20)).toEqual([
      1, "ellipsis-start", 9, 10, 11, "ellipsis-end", 20,
    ]);
  });

  test("always fills seven slots once pages overflow", () => {
    for (const count of [8, 9, 20]) {
      for (let page = 1; page <= count; page++) {
        const items = pageItems(page, count);
        expect(items).toHaveLength(7);
        expect(items).toContain(page);
      }
    }
  });
});
