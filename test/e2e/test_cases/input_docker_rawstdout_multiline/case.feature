@input
Feature: input docker rawstdout multiline
  Test input docker rawstdout multiline

  @e2e @docker-compose
  Scenario: TestInputDockerRawStdoutMultiline
    Given {docker-compose} environment
    Given subcribe data from {grpc} with config
    """
    """
    Given {input-docker-rawstdout-case} local config as below
    """
    enable: true
    inputs:
      - Type: service_docker_stdout_raw
        IncludeEnv:
          STDOUT_SWITCH: "true"
    """
    When start docker-compose {input_docker_rawstdout}
    Then there is at least {1} logs
    Then the log fields match kv
    """
    _time_: ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([zZ]|([\+-])([01]\d|2[0-3]):?([0-5]\d)?)?$
    content: "^hello$"
    _source_: "^stdout$"
    _image_name_: ".*_container:latest$"
    _container_name_: ".*[-_]container[-_]1$"
    _container_ip_: ^\b(?:(?:2(?:[0-4][0-9]|5[0-5])|[0-1]?[0-9]?[0-9])\.){3}(?:(?:2([0-4][0-9]|5[0-5])|[0-1]?[0-9]?[0-9]))\b$
    """