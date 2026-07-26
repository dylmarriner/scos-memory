# Row-Level Security policies

The initial migration enables RLS on sensitive tables but intentionally does not install permissive policies. PostgreSQL therefore denies access until application roles and transaction-local identity settings are defined.

The application SHALL set verified tenant and principal context at the start of each transaction. Policies must be introduced with integration tests proving tenant, namespace and sensitivity isolation before normal application roles are granted access.
