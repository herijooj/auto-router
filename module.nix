{
  lib,
  config,
  pkgs,
  ...
}: let
  cfg = config.services.auto-router;
in {
  options.services.auto-router = {
    enable = lib.mkEnableOption "Auto Router for llama-swap";
    package = lib.mkOption {
      type = lib.types.package;
      description = "The auto-router package to use";
    };
    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "Listen address for the auto router";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 9293;
      description = "Port for the auto router";
    };
    llamaSwapURL = lib.mkOption {
      type = lib.types.str;
      default = "http://127.0.0.1:9292";
      description = "URL of the llama-swap service";
    };
    excludeModels = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = ["lfm2-350m" "glm-ocr"];
      description = "Models to exclude from auto-selection";
    };
    preferredModels = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
      description = "Preferred models for fallback loading";
    };
    healthCheckInterval = lib.mkOption {
      type = lib.types.int;
      default = 10;
      description = "Health check interval in seconds";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.auto-router = {
      description = "Auto Router for llama-swap";
      after = ["network.target" "llama-swap.service"];
      wants = ["llama-swap.service"];
      wantedBy = ["multi-user.target"];

      serviceConfig = {
        Type = "simple";
        User = "llama";
        Group = "users";
        ExecStart = "${cfg.package}/bin/auto-router";
        Environment = [
          "LLAMA_SWAP_URL=${cfg.llamaSwapURL}"
          "LISTEN_ADDR=${cfg.listenAddress}:${toString cfg.port}"
          "EXCLUDE_MODELS=${lib.concatStringsSep "," cfg.excludeModels}"
          "PREFERRED_MODELS=${lib.concatStringsSep "," cfg.preferredModels}"
          "HEALTH_CHECK_INTERVAL=${toString cfg.healthCheckInterval}"
        ];
        Restart = "on-failure";
        RestartSec = "5s";
      };
    };
  };
}
