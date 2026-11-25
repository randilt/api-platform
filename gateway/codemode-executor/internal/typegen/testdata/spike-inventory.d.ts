declare const codemode: {
  inventory: {
    /** Permanently delete an item. (destructive) */
    delete_item(args: { /** the item SKU, e.g. SKU-001 */ sku: string; }): Promise<{ deleted: boolean; }>;
    /** Get the current stock level for one SKU. */
    get_stock(args: { /** the item SKU, e.g. SKU-001 */ sku: string; }): Promise<{ qty: number; sku: string; }>;
    /** List every inventory item with its quantity and warehouse. */
    list_items(args?: {}): Promise<{ items: null | Array<{ name: string; /** units on hand */ qty: number; /** stock keeping unit */ sku: string; /** city of the warehouse holding the item */ warehouse: string; }>; }>;
  };
};
