// GENERATED CODE - DO NOT MODIFY BY HAND

part of 'types.dart';

// **************************************************************************
// TypeAdapterGenerator
// **************************************************************************

class MessageContentURLAdapter extends TypeAdapter<MessageContentURL> {
  @override
  final int typeId = 1;

  @override
  MessageContentURL read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return MessageContentURL(
      url: fields[0] as String,
    );
  }

  @override
  void write(BinaryWriter writer, MessageContentURL obj) {
    writer
      ..writeByte(1)
      ..writeByte(0)
      ..write(obj.url);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is MessageContentURLAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class MessageContentAdapter extends TypeAdapter<MessageContent> {
  @override
  final int typeId = 2;

  @override
  MessageContent read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return MessageContent(
      type: fields[0] as String,
      text: fields[1] as String?,
      imageUrl: fields[2] as MessageContentURL?,
    );
  }

  @override
  void write(BinaryWriter writer, MessageContent obj) {
    writer
      ..writeByte(3)
      ..writeByte(0)
      ..write(obj.type)
      ..writeByte(1)
      ..write(obj.text)
      ..writeByte(2)
      ..write(obj.imageUrl);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is MessageContentAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class MessageAdapter extends TypeAdapter<Message> {
  @override
  final int typeId = 3;

  @override
  Message read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return Message(
      role: fields[0] as String,
      content: (fields[1] as List).cast<MessageContent>(),
      timestamp: fields[2] as DateTime?,
      totalTokens: fields[3] as int?,
      model: fields[4] as Model?,
    );
  }

  @override
  void write(BinaryWriter writer, Message obj) {
    writer
      ..writeByte(5)
      ..writeByte(0)
      ..write(obj.role)
      ..writeByte(1)
      ..write(obj.content)
      ..writeByte(2)
      ..write(obj.timestamp)
      ..writeByte(3)
      ..write(obj.totalTokens)
      ..writeByte(4)
      ..write(obj.model);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is MessageAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class ConversationAdapter extends TypeAdapter<Conversation> {
  @override
  final int typeId = 4;

  @override
  Conversation read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return Conversation(
      id: fields[0] as String,
      title: fields[2] as String,
      lastMessageAt: fields[4] as DateTime,
      modelSelected: fields[6] as ModelSelected,
      createdAt: fields[3] as DateTime?,
      messages: (fields[5] as List).cast<Message>(),
    );
  }

  @override
  void write(BinaryWriter writer, Conversation obj) {
    writer
      ..writeByte(6)
      ..writeByte(0)
      ..write(obj.id)
      ..writeByte(2)
      ..write(obj.title)
      ..writeByte(3)
      ..write(obj.createdAt)
      ..writeByte(4)
      ..write(obj.lastMessageAt)
      ..writeByte(5)
      ..write(obj.messages)
      ..writeByte(6)
      ..write(obj.modelSelected);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is ConversationAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class AttachmentAdapter extends TypeAdapter<Attachment> {
  @override
  final int typeId = 5;

  @override
  Attachment read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return Attachment(
      type: fields[0] as String,
      content: fields[1] as String,
      filename: fields[2] as String?,
      ext: fields[3] as String?,
    );
  }

  @override
  void write(BinaryWriter writer, Attachment obj) {
    writer
      ..writeByte(4)
      ..writeByte(0)
      ..write(obj.type)
      ..writeByte(1)
      ..write(obj.content)
      ..writeByte(2)
      ..write(obj.filename)
      ..writeByte(3)
      ..write(obj.ext);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is AttachmentAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class ModelSelectedAdapter extends TypeAdapter<ModelSelected> {
  @override
  final int typeId = 6;

  @override
  ModelSelected read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return ModelSelected(
      text: fields[0] as Model?,
      vision: fields[1] as Model?,
      imageGen: fields[2] as Model?,
      audioGen: fields[5] as Model?,
      voiceGen: fields[4] as Model?,
      audioTranscribe: fields[3] as Model?,
      videoGen: fields[6] as Model?,
      videoVision: fields[7] as Model?,
      code: fields[8] as Model?,
    );
  }

  @override
  void write(BinaryWriter writer, ModelSelected obj) {
    writer
      ..writeByte(9)
      ..writeByte(0)
      ..write(obj.text)
      ..writeByte(1)
      ..write(obj.vision)
      ..writeByte(2)
      ..write(obj.imageGen)
      ..writeByte(3)
      ..write(obj.audioTranscribe)
      ..writeByte(4)
      ..write(obj.voiceGen)
      ..writeByte(5)
      ..write(obj.audioGen)
      ..writeByte(6)
      ..write(obj.videoGen)
      ..writeByte(7)
      ..write(obj.videoVision)
      ..writeByte(8)
      ..write(obj.code);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is ModelSelectedAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}

class ModelAdapter extends TypeAdapter<Model> {
  @override
  final int typeId = 7;

  @override
  Model read(BinaryReader reader) {
    final numOfFields = reader.readByte();
    final fields = <int, dynamic>{
      for (int i = 0; i < numOfFields; i++) reader.readByte(): reader.read(),
    };
    return Model(
      name: fields[0] as String,
      params: (fields[1] as Map?)?.cast<String, String>(),
    );
  }

  @override
  void write(BinaryWriter writer, Model obj) {
    writer
      ..writeByte(2)
      ..writeByte(0)
      ..write(obj.name)
      ..writeByte(1)
      ..write(obj.params);
  }

  @override
  int get hashCode => typeId.hashCode;

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is ModelAdapter &&
          runtimeType == other.runtimeType &&
          typeId == other.typeId;
}
