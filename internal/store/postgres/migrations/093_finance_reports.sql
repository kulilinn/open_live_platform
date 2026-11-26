CREATE INDEX finance_purchase_daily_archive ON legacy_business_history((record->>'create_time'))
    WHERE source_table='order_statistic_data';

CREATE INDEX finance_legacy_order_number ON legacy_business_history(owner_id,(record->>'no'))
    WHERE source_table='pay_orders';

CREATE INDEX finance_large_order_number ON payment_large_order_attempts(user_id,provider_order)
    WHERE provider_order IS NOT NULL;
