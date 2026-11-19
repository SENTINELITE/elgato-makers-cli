# Discount plans

`maker discounts prepare --file discount.json` validates a JSON file offline and saves an immutable 24-hour plan. Take the organization and product IDs from your Maker catalog. A plan for a specific product checks that product's ownership before writing to the Marketplace.

| Discount | `mode` | `scope` | Terms |
| --- | --- | --- | --- |
| Percentage across your products | `percentage` | `all_products` | `percentage_off`, an integer from 1 to 100 |
| Fixed amount across your products | `fixed` | `all_products` | `amount_minor` and `currency: "USD"`. Use whole dollars in cents, from `100` through `2147483600` in increments of `100`. For example, `500` is USD 5. |
| Percentage for one product | `percentage` | `specific_products` | `percentage_off` and exactly one entry in `product_ids` |

A fixed-amount plan can omit `percentage_off` or set it to zero. Omit `amount_minor` and `currency` on a percentage plan. Omit `product_ids` on all-products plans. Maker can apply its own amount limits. Other currencies, fractional dollar amounts, multiple targets, fixed amounts on a specific product, and product-type targeting are outside the supported write shapes.

All plans include `organization_id`, `name`, `maker_code`, `mode`, `scope`, a UTC whole-minute `start_at`, and explicit `end_at`, `max_unique_users`, and `max_uses_per_user` values. Use an alphanumeric `maker_code` of 1 to 50 characters. The end must be a UTC whole minute after the start, and limits must be positive integers. Use JSON `null` for no end or unlimited use. A synthetic fixed-amount example follows.

```json
{
  "organization_id": "00000000-0000-4000-8000-000000000001",
  "name": "Example five-dollar discount",
  "maker_code": "EXAMPLE5",
  "mode": "fixed",
  "scope": "all_products",
  "amount_minor": 500,
  "currency": "USD",
  "start_at": "2030-01-01T00:00:00Z",
  "end_at": "2030-01-05T00:00:00Z",
  "max_unique_users": null,
  "max_uses_per_user": 1
}
```

To use the same fields for a selected-product percentage discount, set `"mode": "percentage"`, `"scope": "specific_products"`, `"percentage_off": 20` and `"product_ids": ["00000000-0000-4000-8000-000000000002"]`. Remove `amount_minor` and `currency`.

Creation and deactivation each require two default-No confirmations or their own explicit `--yes`/`-y`. Deactivation also requires the exact current maker-code suffix. If creation has an uncertain outcome, run `maker discounts status PLAN_DIGEST` to reconcile the exact saved terms before attempting another creation. Use `maker discounts show DISCOUNT_ID` to inspect a deactivation. Read output keeps the provider's values and nullable limits. A deactivated discount can still report `valid=true`.
