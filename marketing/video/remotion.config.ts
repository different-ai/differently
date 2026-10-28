import {Config} from '@remotion/cli/config';

Config.setVideoImageFormat('png');
Config.setCodec('h264');
Config.setCrf(16);
Config.setPixelFormat('yuv420p');
Config.setAudioBitrate('320k');
Config.setConcurrency(8);
Config.setEntryPoint('src/index.ts');
Config.setColorSpace('bt709');
Config.setX264Preset('slow');
