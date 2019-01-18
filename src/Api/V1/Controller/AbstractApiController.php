<?php

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ApiResponse;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\{JsonResponse, Response};

abstract class AbstractApiController extends AbstractController
{
    protected const DEFAULT_SERIALIZER_GROUPS = ['api'];

    protected function createJsonResponse($data, array $groups = [], int $code = Response::HTTP_OK, string $message = null, string $status = ''): JsonResponse
    {
        return $this->json(new ApiResponse($data, $code, $message, $status), $code, [], [
            'groups' => array_merge(self::DEFAULT_SERIALIZER_GROUPS,$groups),
        ]);
    }
}