{ lib, buildGoModule }:
buildGoModule {
  pname = "kiseki";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  vendorHash = "sha256-7IC/p5GlD2EZkDXQzkaZ7E19S/ABKEBsg68vt8pykis=";

  env.CGO_ENABLED = "0";
  subPackages = [ "cmd/kiseki" ];
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Personal lifelog collection and archive";
    mainProgram = "kiseki";
  };
}
