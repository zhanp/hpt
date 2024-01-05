# HPT

HTTP performance tool to benchmark.

```text
Usage: demo [options...] <url>
  -c Maximum concurrency (default 1)
  -duration Duration of test (default 30s)
  -ramp_up_duration Time taken to gradually increase to the maximum concurrency
  -X Specify request method to use (default "GET")
  -H HTTP headers
  -d HTTP POST data
  -script todo
  -max_redirects Maximum redirect times
  -debug Enable debug log
  -log_path File path for logging (default "stdout")
```